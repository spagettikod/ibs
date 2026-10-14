package site

import (
	"context"
	"crypto/ed25519"
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

	"ibs/internal/blockstore"
	"ibs/internal/config"
	"ibs/internal/db"
	"ibs/internal/keys"
	"ibs/internal/relay"
)

// RestoreOptions name the recovery phrase and where to fetch the metadata
// backup: one of the site's own storage nodes, or a former peer (by its
// direct address, or through a relay by its site ID).
type RestoreOptions struct {
	Phrase       string
	NodeAddress  string
	PeerAddress  string
	RelayAddress string
	RelayToken   string
	PeerSiteID   string
	Password     string
}

// Restore rebuilds a lost site from its recovery phrase. It fetches the
// encrypted metadata backup from a peer (which recognizes the site by its
// key), checks the other peers for a newer generation, and resets local
// state. Block data is healed from replicas in the background afterwards.
func Restore(ctx context.Context, cfg config.Config, opt RestoreOptions) (*Site, error) {
	if IsSetUp(cfg.DataDir) {
		return nil, errors.New("site is already set up")
	}
	if len(opt.Password) < 8 {
		return nil, errors.New("admin password must be at least 8 characters")
	}
	if opt.NodeAddress == "" && opt.PeerAddress == "" && (opt.RelayToken == "" || opt.PeerSiteID == "") {
		return nil, errors.New("give the address of one of the site's storage nodes, the address of a former peer, or a relay token and the peer's site ID")
	}
	seed, err := keys.DecodePhrase(opt.Phrase)
	if err != nil {
		return nil, err
	}
	id, err := keys.FromSeed(seed)
	if err != nil {
		return nil, err
	}
	cert, err := id.Certificate()
	if err != nil {
		return nil, err
	}
	// A relay lets the restored site reach peers without a direct address.
	// It recognizes the site's key from before, so the token is enough.
	var rc *relay.Client
	if opt.RelayToken != "" {
		t, err := relay.ParseToken(opt.RelayToken)
		if err != nil {
			return nil, err
		}
		if a := relayAddr(opt.RelayAddress); a != "" {
			t.Address = a
		}
		if t.Address == "" {
			return nil, errors.New("the relay's address is required (host:7444)")
		}
		rc = &relay.Client{Token: t, Cert: cert, Name: "restore"}
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go rc.Run(rctx)
		for i := 0; i < 75 && !rc.Connected(); i++ {
			time.Sleep(200 * time.Millisecond)
		}
		if !rc.Connected() {
			return nil, fmt.Errorf("cannot connect to relay %s: %s", t.Address, rc.Status().Error)
		}
	}
	// via returns an HTTP client and base URL for a peer: direct when it has
	// an address, otherwise through the relay.
	via := func(addr, siteID string, pin ed25519.PublicKey, seen *ed25519.PublicKey) (*http.Client, string) {
		h := pinnedHTTP(cert, pin, seen)
		if addr != "" || rc == nil {
			return h, peerURL(addr, "")
		}
		h.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return rc.Dial(ctx, siteID)
		}
		return h, "https://peer.ibs"
	}
	// fromNode fetches the snapshot a storage node keeps. The node only
	// answers this site's key, which the phrase restores.
	fromNode := func(addr string, pin ed25519.PublicKey, seen *ed25519.PublicKey) (int64, []byte, error) {
		h := pinnedHTTP(cert, pin, seen)
		addr = strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, "7480")
		}
		return blockstore.NewRemote("https://"+addr, h).GetMetadata(ctx)
	}
	var seen ed25519.PublicKey
	var gen int64
	var blob []byte
	if opt.NodeAddress != "" {
		gen, blob, err = fromNode(opt.NodeAddress, nil, &seen)
		if err != nil {
			return nil, fmt.Errorf("fetch metadata from node %s: %w", opt.NodeAddress, err)
		}
	} else {
		h, base := via(opt.PeerAddress, opt.PeerSiteID, nil, &seen)
		source := opt.PeerAddress
		if source == "" {
			source = "site " + opt.PeerSiteID
		}
		if gen, blob, err = fetchBackup(ctx, h, base); err != nil {
			return nil, fmt.Errorf("fetch backup from %s: %w", source, err)
		}
	}
	raw, err := keys.Open(id.Master, blob, backupAAD(id.SiteID))
	if err != nil {
		return nil, errors.New("the backup could not be decrypted with this recovery phrase")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(cfg.DataDir, dbFile)
	if err := writeDB(dbPath, raw); err != nil {
		return nil, err
	}
	d, err := db.Open(dbPath)
	if err != nil {
		return nil, err
	}

	// The source must be one of this site's nodes or peers.
	var stored []byte
	var known error
	if opt.NodeAddress != "" {
		known = d.QueryRow(`SELECT pubkey FROM nodes WHERE pubkey = ?`, []byte(seen)).Scan(&stored)
	} else {
		known = d.QueryRow(`SELECT pubkey FROM peers WHERE site_id = ?`, keys.SiteID(seen)).Scan(&stored)
	}
	if known != nil || !seen.Equal(ed25519.PublicKey(stored)) {
		d.Close()
		os.Remove(dbPath)
		return nil, errors.New("the backup source is not a known node or peer of this site")
	}
	// replace switches to a newer snapshot found elsewhere.
	replace := func(g int64, b []byte) error {
		r, err := keys.Open(id.Master, b, backupAAD(id.SiteID))
		if err != nil {
			return nil // not ours; ignore
		}
		d.Close()
		if err := writeDB(dbPath, r); err != nil {
			return err
		}
		if d, err = db.Open(dbPath); err != nil {
			return err
		}
		gen = g
		return nil
	}

	// The site's own storage nodes may hold a newer generation.
	type nodeRow struct {
		addr string
		pub  ed25519.PublicKey
	}
	var nodes []nodeRow
	if rows, err := d.Query(`SELECT address, pubkey FROM nodes WHERE is_local = 0 AND length(pubkey) = ?`, ed25519.PublicKeySize); err == nil {
		for rows.Next() {
			var n nodeRow
			var pub []byte
			rows.Scan(&n.addr, &pub)
			n.pub = pub
			nodes = append(nodes, n)
		}
		rows.Close()
	}
	for _, n := range nodes {
		if g, b, err := fromNode(n.addr, n.pub, nil); err == nil && g > gen {
			if err := replace(g, b); err != nil {
				return nil, err
			}
		}
	}

	// Another peer may hold a newer generation.
	type peerRow struct {
		id, addr string
		pub      ed25519.PublicKey
	}
	var others []peerRow
	rows, err := d.Query(`SELECT site_id, address, pubkey FROM peers WHERE site_id != ?`, keys.SiteID(seen))
	if err == nil {
		for rows.Next() {
			var p peerRow
			var pub []byte
			rows.Scan(&p.id, &p.addr, &pub)
			p.pub = pub
			others = append(others, p)
		}
		rows.Close()
	}
	for _, p := range others {
		h, base := via(p.addr, p.id, p.pub, nil)
		g, b, err := fetchBackup(ctx, h, base)
		if err != nil || g <= gen {
			continue
		}
		if err := replace(g, b); err != nil {
			return nil, err
		}
	}

	// Reset state that belonged to the lost installation.
	resets := []string{
		`DELETE FROM held_blocks`,
		`DELETE FROM peer_backups`,
		`DELETE FROM invites`,
		`UPDATE blocks SET node_id = '' WHERE node_id = '` + LocalNodeID + `'`,
		`UPDATE blocks SET verified = 0`,
		`UPDATE peers SET online = 0`,
	}
	for _, q := range resets {
		if _, err := d.Exec(q); err != nil {
			d.Close()
			return nil, err
		}
	}
	if rc != nil {
		t := rc.CurrentToken()
		d.Exec(`INSERT INTO relays(id, address, pubkey, secret, added) VALUES(?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET address = excluded.address, secret = excluded.secret`,
			relay.ID(t.Key), t.Address, []byte(t.Key), t.Secret, time.Now().Unix())
	}
	db.SetSetting(d, "admin_password", keys.HashPassword(opt.Password))
	db.SetSetting(d, "restored", strconv.FormatInt(time.Now().Unix(), 10))
	db.SetSetting(d, "generation", strconv.FormatInt(gen, 10))
	d.Close()

	if err := os.WriteFile(filepath.Join(cfg.DataDir, identityFile), seed, 0o600); err != nil {
		return nil, err
	}
	s, err := Open(cfg)
	if err != nil {
		return nil, err
	}
	s.markDirty()
	s.log.Info("site restored from recovery phrase", "generation", gen, "peers", len(others)+1)
	return s, nil
}

func writeDB(path string, raw []byte) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
	tmp := path + ".restore"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fetchBackup(ctx context.Context, h *http.Client, base string) (int64, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/peer/v1/backup", nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := h.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, nil, errors.New("the peer holds no backup for this site")
	}
	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("peer returned %s", resp.Status)
	}
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return parseInt(resp.Header.Get("X-IBS-Generation")), blob, nil
}
