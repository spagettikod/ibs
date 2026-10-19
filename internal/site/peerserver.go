package site

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ibs/internal/blockcodec"
	"ibs/internal/blockstore"
	"ibs/internal/node"
	"ibs/internal/version"
)

type ctxKey int

const peerKey ctxKey = 0

type caller struct {
	siteID string
	pub    ed25519.PublicKey
	paired bool
}

func callerOf(r *http.Request) caller { return r.Context().Value(peerKey).(caller) }

// peerHandler serves the site-to-site API. The same handler is used for
// every way in: TCP (direct address and the site's own storage nodes),
// connections relayed by a relay, and direct QUIC connections.
//
// Unpaired sites may only redeem an invite or fetch their own backup (for a
// restore). Storage nodes may only register over TCP, i.e. from the site's
// own network, never through a relay or QUIC.
func (s *Site) peerHandler() http.Handler {
	mux := http.NewServeMux()
	paired := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !callerOf(r).paired {
				http.Error(w, "site is not paired with "+s.Name(), http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /peer/v1/pair", s.handlePair)
	mux.HandleFunc("POST /peer/v1/invite", s.handleInviteFetch) // a long invite's details, for its secret
	mux.HandleFunc("POST /cluster/v1/register", s.handleNodeRegister)
	mux.HandleFunc("POST /peer/v1/leave", paired(s.handleLeave))
	mux.HandleFunc("GET /peer/v1/status", paired(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.Status())
	}))
	mux.HandleFunc("GET /peer/v1/probe", paired(s.handleProbe))
	mux.HandleFunc("PUT /peer/v1/blocks/{id}", paired(s.handlePutHeld))
	mux.HandleFunc("GET /peer/v1/blocks/{id}", paired(s.handleGetHeld))
	mux.HandleFunc("POST /peer/v1/blocks/delete", paired(s.handleDeleteHeld))
	mux.HandleFunc("POST /peer/v1/blocks/release", paired(s.handleRelease))
	mux.HandleFunc("GET /peer/v1/inventory", paired(s.handleInventory))
	mux.HandleFunc("PUT /peer/v1/backup", paired(s.handlePutBackup))
	mux.HandleFunc("GET /peer/v1/backup", s.handleGetBackup) // also used by an unpaired restore client
	mux.HandleFunc("GET /peer/v1/releases/{version}", paired(s.handleGetRelease))
	mux.HandleFunc("POST /peer/v1/scrub/check", paired(s.handleCopyCheck))
	mux.HandleFunc("GET /cluster/v1/release", s.handleNodeRelease)
	mux.HandleFunc("GET /cluster/v1/releases/{version}", func(w http.ResponseWriter, r *http.Request) {
		if !s.isNodeKey(callerOf(r).pub) {
			http.Error(w, "not a storage node of this site", http.StatusForbidden)
			return
		}
		s.handleGetRelease(w, r)
	})
	mux.HandleFunc("HEAD /peer/v1/backup", paired(s.handleGetBackup))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			http.Error(w, "tls required", http.StatusBadRequest)
			return
		}
		id, pub, ok := s.verifyPeerCert(r.TLS.PeerCertificates)
		if pub == nil {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		if !ok && s.isFormerMember(id) {
			// This site left the IBS; say so, so the caller forgets it.
			http.Error(w, fmt.Sprintf("%s%d: %s has left the IBS", leftPrefix, s.SettingInt("left_at"), s.Name()), http.StatusForbidden)
			return
		}
		if !ok && !strings.HasPrefix(r.URL.Path, "/cluster/") && s.admitMember(pub) {
			ok = true // a member of the IBS this site hadn't connected to yet
		}
		origin := originOf(r.Context())
		if strings.HasPrefix(r.URL.Path, "/cluster/") && origin != originTCP {
			http.Error(w, "storage nodes must register over the local network", http.StatusForbidden)
			return
		}
		if !ok && origin != originTCP && !(r.Method == http.MethodPost && (r.URL.Path == "/peer/v1/pair" || r.URL.Path == "/peer/v1/invite")) &&
			!(r.Method == http.MethodGet && r.URL.Path == "/peer/v1/backup") {
			http.Error(w, "site is not paired with "+s.Name(), http.StatusForbidden)
			return
		}
		if ok && origin == originTCP {
			if adv := r.Header.Get("X-IBS-Address"); adv != "" {
				s.DB.Exec(`UPDATE peers SET address = ? WHERE site_id = ? AND address != ?`, adv, id, adv)
			}
		}
		ctx := context.WithValue(r.Context(), peerKey, caller{siteID: id, pub: pub, paired: ok})
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

// probeData is sent by the speed probe; random so nothing can compress it.
var probeData = func() []byte {
	b := make([]byte, 1<<20)
	rand.Read(b)
	return b
}()

// handleProbe sends up to 8 MiB of random bytes, used to measure the speed
// of the path between two sites.
func (s *Site) handleProbe(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
	n = min(max(n, 0), 8<<20)
	w.Header().Set("Content-Length", strconv.Itoa(n))
	for n > 0 {
		k := min(n, len(probeData))
		if _, err := w.Write(probeData[:k]); err != nil {
			return
		}
		n -= k
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Site) handlePair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp, err := s.redeemInvite(callerOf(r).pub, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	writeJSON(w, resp)
}

func (s *Site) handleNodeRegister(w http.ResponseWriter, r *http.Request) {
	var req node.Registration
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "bad remote address", http.StatusBadRequest)
		return
	}
	reply, err := s.RegisterNode(callerOf(r).pub, ip, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	writeJSON(w, reply)
}

// handleNodeRelease gives a storage node's launcher this manager's own
// version on the node's first start, with the release keys to trust it by,
// so the node needs nothing from GitHub. The node proves it may join with the
// join secret (header X-IBS-Join-Secret), or is already one of this site's.
func (s *Site) handleNodeRelease(w http.ResponseWriter, r *http.Request) {
	want := s.Setting("join_token")
	secret := r.Header.Get("X-IBS-Join-Secret")
	if !s.isNodeKey(callerOf(r).pub) && (want == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(want)) != 1) {
		http.Error(w, "the join token is not valid for this site", http.StatusForbidden)
		return
	}
	b, err := s.ReleaseBundle(version.Version)
	if err != nil {
		http.Error(w, "this manager has no signed release of the version it runs ("+version.Version+"); it wasn't started by the IBS launcher", http.StatusNotFound)
		return
	}
	w.Header().Set("X-IBS-Trusted-Keys", strings.Join(s.trustedKeyStrings(), ","))
	w.Header().Set("Content-Type", "application/x-tar")
	w.Write(b)
}

// handleLeave: the calling site left the IBS.
func (s *Site) handleLeave(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	var req struct {
		At int64 `json:"at"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req)
	if req.At == 0 {
		req.At = time.Now().Unix()
	}
	go s.memberLeft(context.Background(), c.siteID, req.At)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Site) handlePutHeld(w http.ResponseWriter, r *http.Request) {
	owner := callerOf(r).siteID
	id := r.PathValue("id")
	if !blockcodec.ValidID(id) {
		http.Error(w, "invalid block id", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, blockcodec.BlockSize+1024))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !blockcodec.Verify(id, data) {
		http.Error(w, "block content does not match id", http.StatusBadRequest)
		return
	}
	var existing int
	s.DB.QueryRow(`SELECT COUNT(*) FROM held_blocks WHERE owner = ? AND id = ?`, owner, id).Scan(&existing)
	if existing > 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	unlock := s.lockBlock(id)
	defer unlock()
	if _, free := s.offeredStorage(); free < int64(len(data)) {
		http.Error(w, "no storage available for peers", http.StatusInsufficientStorage)
		return
	}
	nodeID, err := s.putOnNode(r.Context(), owner, id, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
		return
	}
	now := time.Now().Unix()
	if _, err := s.DB.Exec(`INSERT INTO held_blocks(owner, id, size, node_id, stored, verified) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner, id) DO UPDATE SET node_id = excluded.node_id, verified = excluded.verified`,
		owner, id, len(data), nodeID, now, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.receivedCopy(owner, len(data))
	s.notify(TopicReplication)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Site) handleGetHeld(w http.ResponseWriter, r *http.Request) {
	owner := callerOf(r).siteID
	id := r.PathValue("id")
	var nodeID string
	if err := s.DB.QueryRow(`SELECT node_id FROM held_blocks WHERE owner = ? AND id = ?`, owner, id).Scan(&nodeID); err != nil {
		http.NotFound(w, r)
		return
	}
	store := s.nodeStore(nodeID)
	if store == nil {
		http.Error(w, "storage node unavailable", http.StatusServiceUnavailable)
		return
	}
	data, err := store.Get(r.Context(), owner, id)
	if errors.Is(err, blockstore.ErrNotFound) || err == nil && !blockcodec.Verify(id, data) {
		s.DB.Exec(`DELETE FROM held_blocks WHERE owner = ? AND id = ?`, owner, id)
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

func (s *Site) handleDeleteHeld(w http.ResponseWriter, r *http.Request) {
	owner := callerOf(r).siteID
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, id := range req.IDs {
		var nodeID string
		if err := s.DB.QueryRow(`SELECT node_id FROM held_blocks WHERE owner = ? AND id = ?`, owner, id).Scan(&nodeID); err != nil {
			continue
		}
		if st := s.nodeStore(nodeID); st != nil {
			if err := st.Delete(r.Context(), owner, id); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		s.DB.Exec(`DELETE FROM held_blocks WHERE owner = ? AND id = ?`, owner, id)
	}
	s.notify(TopicReplication)
	w.WriteHeader(http.StatusNoContent)
}

type InventoryItem struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

// handleInventory lists every block of the caller that we hold and that
// passed our last integrity check.
func (s *Site) handleInventory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT id, size FROM held_blocks WHERE owner = ? ORDER BY id`, callerOf(r).siteID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	io.WriteString(w, "[")
	first := true
	for rows.Next() {
		var it InventoryItem
		rows.Scan(&it.ID, &it.Size)
		if !first {
			io.WriteString(w, ",")
		}
		first = false
		enc.Encode(it)
	}
	io.WriteString(w, "]")
}

func backupPath(dataDir, owner string) string {
	return filepath.Join(dataDir, "peer-backups", owner+".bin")
}

func removeBackupFile(dataDir, owner string) {
	if blockstore.ValidName(owner) {
		os.Remove(backupPath(dataDir, owner))
	}
}

func (s *Site) handlePutBackup(w http.ResponseWriter, r *http.Request) {
	owner := callerOf(r).siteID
	gen, err := strconv.ParseInt(r.URL.Query().Get("generation"), 10, 64)
	if err != nil {
		http.Error(w, "generation required", http.StatusBadRequest)
		return
	}
	var cur int64 = -1
	s.DB.QueryRow(`SELECT generation FROM peer_backups WHERE owner = ?`, owner).Scan(&cur)
	if gen < cur {
		http.Error(w, fmt.Sprintf("have newer generation %d", cur), http.StatusConflict)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p := backupPath(s.cfg.DataDir, owner)
	os.MkdirAll(filepath.Dir(p), 0o700)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.DB.Exec(`INSERT INTO peer_backups(owner, generation, size, updated) VALUES(?, ?, ?, ?)
		ON CONFLICT(owner) DO UPDATE SET generation = excluded.generation, size = excluded.size, updated = excluded.updated`,
		owner, gen, len(data), time.Now().Unix())
	w.WriteHeader(http.StatusNoContent)
}

// handleGetBackup returns the caller's own encrypted metadata backup. The
// caller is identified by its certificate key, so this also serves a site
// being restored from its recovery phrase. Only the owner can decrypt it.
func (s *Site) handleGetBackup(w http.ResponseWriter, r *http.Request) {
	owner := callerOf(r).siteID
	var gen int64
	if err := s.DB.QueryRow(`SELECT generation FROM peer_backups WHERE owner = ?`, owner).Scan(&gen); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-IBS-Generation", strconv.FormatInt(gen, 10))
	if r.Method == http.MethodHead {
		return
	}
	data, err := os.ReadFile(backupPath(s.cfg.DataDir, owner))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

// handleGetRelease sends an installed version as an .ibsrelease file. The
// receiver checks the signature against its own trusted keys.
func (s *Site) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	data, err := s.ReleaseBundle(r.PathValue("version"))
	if err != nil {
		http.Error(w, "no such release", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Write(data)
}
