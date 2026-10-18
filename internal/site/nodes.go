package site

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"database/sql"
	"errors"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"ibs/internal/blockstore"
	"ibs/internal/config"
	"ibs/internal/node"
	"ibs/internal/version"
)

// nodeTimeout is how long a storage node may go without a heartbeat before
// the manager stops placing blocks on it.
const nodeTimeout = 60 * time.Second

// ErrNoSpace means no online node has room for a block within its limit.
var ErrNoSpace = errors.New("no storage node at this site has free space within its storage limit")

type NodeInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Address      string `json:"address"`
	Quota        int64  `json:"quota"`          // limit on IBS data on this node, 0 = whole disk
	LimitFromEnv bool   `json:"limit_from_env"` // set with IBS_STORAGE_LIMIT on the node, not changeable here
	Version      string `json:"version"`
	DiskCapacity int64  `json:"disk_capacity"` // as reported by the node
	DiskFree     int64  `json:"disk_free"`
	Capacity     int64  `json:"capacity"` // effective, within the quota
	Free         int64  `json:"free"`
	LastSeen     int64  `json:"last_seen"`
	Local        bool   `json:"local"`
	Online       bool   `json:"online"`
	Lost         bool   `json:"lost"`                // offline beyond the grace period: blocks are being restored elsewhere
	Stranded     int64  `json:"stranded"`            // blocks on this node with no replica on another site
	MetadataGen  int64  `json:"metadata_generation"` // metadata snapshot pushed to it (-1 none; manager: current)
	Counted      bool   `json:"counted"`             // the counts below are in (stats.go)
	Blocks       int64  `json:"blocks"`              // this site's blocks on it
	HeldBlocks   int64  `json:"held_blocks"`         // copies it keeps for other sites
	Used         int64  `json:"used"`
}

func (s *Site) loadNodes() error {
	rows, err := s.DB.Query(`SELECT id, address, pubkey FROM nodes WHERE is_local = 0 AND length(pubkey) = ?`, ed25519.PublicKeySize)
	if err != nil {
		return err
	}
	defer rows.Close()
	s.nodesMu.Lock()
	defer s.nodesMu.Unlock()
	for rows.Next() {
		var id, addr string
		var pub []byte
		if err := rows.Scan(&id, &addr, &pub); err != nil {
			return err
		}
		client, err := s.newPinnedHTTP(pub, nil)
		if err != nil {
			return err
		}
		s.nodes[id] = blockstore.NewRemote(addr, client)
	}
	return rows.Err()
}

func (s *Site) nodeStore(id string) blockstore.Store {
	s.nodesMu.RLock()
	defer s.nodesMu.RUnlock()
	return s.nodes[id]
}

// RegisterNode handles a storage node's registration and heartbeats. The
// node proves its key with its TLS client certificate; a node not known yet
// must also present the join secret. The manager reaches the node at the
// IP the connection came from, unless the node advertises an address.
func (s *Site) RegisterNode(pub ed25519.PublicKey, remoteIP string, req node.Registration) (*node.RegistrationReply, error) {
	if !blockstore.ValidName(req.ID) || req.ID == LocalNodeID || req.Port <= 0 || req.Port > 65535 {
		return nil, errors.New("invalid node registration")
	}
	var stored []byte
	var lastSeen int64
	err := s.DB.QueryRow(`SELECT pubkey, last_seen FROM nodes WHERE id = ?`, req.ID).Scan(&stored, &lastSeen)
	returning := lastSeen < time.Now().Add(-nodeTimeout).Unix()
	known := err == nil && pub.Equal(ed25519.PublicKey(stored))
	if !known {
		if err == nil && len(stored) == ed25519.PublicKeySize {
			return nil, errors.New("this node id is registered with a different key")
		}
		want := s.Setting("join_token")
		if want == "" || subtle.ConstantTimeCompare([]byte(req.Secret), []byte(want)) != 1 {
			return nil, errors.New("invalid join token")
		}
	}
	host := req.Advertise
	if host == "" {
		host = net.JoinHostPort(remoteIP, strconv.Itoa(req.Port))
	}
	address := "https://" + strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	name := req.Name
	if name == "" {
		name = req.ID
	}
	_, err = s.DB.Exec(`INSERT INTO nodes(id, name, address, secret, pubkey, last_seen, is_local, version)
		VALUES(?, ?, ?, '', ?, ?, 0, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, address = excluded.address,
			pubkey = excluded.pubkey, last_seen = excluded.last_seen, version = excluded.version`,
		req.ID, name, address, []byte(pub), time.Now().Unix(), req.Version)
	if err != nil {
		return nil, err
	}
	if err := s.updateNodeStorage(req.ID, max(req.Limit, config.NoStorage), req.Capacity, req.Free); err != nil {
		return nil, err
	}
	s.nodesMu.Lock()
	if r, ok := s.nodes[req.ID].(*blockstore.Remote); !ok || r.BaseURL != address || !known {
		client, err := s.newPinnedHTTP(pub, nil)
		if err != nil {
			s.nodesMu.Unlock()
			return nil, err
		}
		s.nodes[req.ID] = blockstore.NewRemote(address, client)
		s.log.Info("storage node registered", "node", req.ID, "name", name, "address", address)
	}
	s.nodesMu.Unlock()
	if !known {
		s.markDirty()
	}
	if returning {
		// It may have lost its disk meanwhile: push metadata again.
		s.peerMu.Lock()
		delete(s.nodeMetaGen, req.ID)
		s.peerMu.Unlock()
		go s.reconcileNode(context.Background(), req.ID)
	}
	return &node.RegistrationReply{SiteName: s.Name(), Address: address,
		TargetVersion: s.nodeTarget(req.ID, req.Version), TrustedKeys: s.trustedKeyStrings()}, nil
}

// RemoveNode forgets a storage node. Blocks it held are re-fetched from
// replicas by the healer.
func (s *Site) RemoveNode(id string) error {
	if id == LocalNodeID {
		return errors.New("the manager node cannot be removed")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM nodes WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE blocks SET node_id = '' WHERE node_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM held_blocks WHERE node_id = ?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.nodesMu.Lock()
	delete(s.nodes, id)
	s.nodesMu.Unlock()
	s.markDirty()
	return nil
}

func (s *Site) refreshLocalNode(ctx context.Context) {
	capacity, free, err := s.nodeStore(LocalNodeID).Stat(ctx)
	if err != nil {
		s.log.Warn("stat local storage", "err", err)
		return
	}
	s.DB.Exec(`UPDATE nodes SET last_seen = ?, version = ? WHERE id = ?`, time.Now().Unix(), version.Version, LocalNodeID)
	s.updateNodeStorage(LocalNodeID, s.cfg.StorageLimit, capacity, free)
}

// nodeUsedCached is nodeUsed from the cached figures (stats.go) when there
// are any: it's asked on every heartbeat, and counting takes a while on large
// sites.
func (s *Site) nodeUsedCached(id string) int64 {
	if s.statsRunning.Load() {
		if st := s.statsCache.Load(); st != nil {
			return st.Nodes[id].Used
		}
	}
	return s.nodeUsed(id)
}

// nodeUsed returns the bytes IBS stores on a node: this site's blocks and
// blocks held for other sites.
func (s *Site) nodeUsed(id string) int64 {
	var own, held int64
	s.DB.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM blocks WHERE node_id = ?`, id).Scan(&own)
	s.DB.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM held_blocks WHERE node_id = ?`, id).Scan(&held)
	return own + held
}

// updateNodeStorage records a node's disk figures and IBS_STORAGE_LIMIT
// (envLimit: 0 = not set, negative = nothing) and derives the capacity and
// free space IBS may use on it. The limit in effect is envLimit when set,
// otherwise the one set in the web interface, otherwise the whole disk.
// Free space is capped by both the remaining limit and the free disk space.
// A limit below the current use deletes nothing; the node just receives no
// new blocks.
func (s *Site) updateNodeStorage(id string, envLimit, diskCapacity, diskFree int64) error {
	quota := envLimit
	if quota == 0 {
		var ui sql.NullInt64
		s.DB.QueryRow(`SELECT ui_limit FROM nodes WHERE id = ?`, id).Scan(&ui)
		quota = ui.Int64
	}
	// What IBS can use is what it stores on the node plus the disk's free
	// space, not the disk's size: the disk may hold other things too. Without
	// a limit that's only shown, so the cached figure will do; a limit is
	// enforced with what IBS stores there exactly.
	capacity, free := min(diskCapacity, s.nodeUsedCached(id)+diskFree), diskFree
	if quota < 0 {
		capacity, free = 0, 0 // shares nothing; existing blocks stay until moved or removed
	} else if quota > 0 {
		used := s.nodeUsed(id)
		capacity = min(quota, used+diskFree)
		free = min(diskFree, max(quota-used, 0))
	}
	_, err := s.DB.Exec(`UPDATE nodes SET quota = ?, env_limit = ?, disk_capacity = ?, disk_free = ?, capacity = ?, free = ? WHERE id = ?`,
		quota, envLimit, diskCapacity, diskFree, capacity, free, id)
	return err
}

// SetNodeLimit sets how much of a node's disk IBS may use, from the web
// interface: 0 = the whole disk, config.NoStorage = nothing (its blocks move
// to other nodes). It is refused when IBS_STORAGE_LIMIT is set on the node.
func (s *Site) SetNodeLimit(id string, limit int64) error {
	var envLimit, diskCapacity, diskFree int64
	err := s.DB.QueryRow(`SELECT env_limit, disk_capacity, disk_free FROM nodes WHERE id = ?`, id).Scan(&envLimit, &diskCapacity, &diskFree)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("no such node")
	} else if err != nil {
		return err
	}
	if envLimit != 0 {
		return errors.New("this node's limit is set with IBS_STORAGE_LIMIT; change it there")
	}
	var ui any
	if limit != 0 {
		ui = limit
	}
	if _, err := s.DB.Exec(`UPDATE nodes SET ui_limit = ? WHERE id = ?`, ui, id); err != nil {
		return err
	}
	if err := s.updateNodeStorage(id, 0, diskCapacity, diskFree); err != nil {
		return err
	}
	s.log.Info("node storage limit changed", "node", id, "limit", limit)
	s.markDirty()
	s.notify(TopicNodes)
	return nil
}

func (s *Site) Nodes() ([]NodeInfo, error) {
	st := s.stats() // the counts, from the cache
	rows, err := s.DB.Query(`SELECT n.id, n.name, n.address, n.quota, n.env_limit != 0, n.version, n.disk_capacity, n.disk_free, n.capacity, n.free, n.last_seen, n.is_local
		FROM nodes n ORDER BY n.is_local DESC, n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cutoff := time.Now().Add(-nodeTimeout).Unix()
	lostCutoff := time.Now().Add(-s.cfg.NodeGrace).Unix()
	var out []NodeInfo
	for rows.Next() {
		var n NodeInfo
		if err := rows.Scan(&n.ID, &n.Name, &n.Address, &n.Quota, &n.LimitFromEnv, &n.Version, &n.DiskCapacity, &n.DiskFree, &n.Capacity, &n.Free, &n.LastSeen, &n.Local); err != nil {
			return nil, err
		}
		if st != nil {
			c := st.Nodes[n.ID]
			n.Counted, n.Blocks, n.HeldBlocks, n.Used, n.Stranded = true, c.Blocks, c.HeldBlocks, c.Used, c.Stranded
		}
		n.Online = n.LastSeen >= cutoff
		n.Lost = !n.Local && n.LastSeen < lostCutoff
		n.MetadataGen = -1
		s.peerMu.Lock()
		if g, ok := s.nodeMetaGen[n.ID]; ok {
			n.MetadataGen = g
		}
		s.peerMu.Unlock()
		if n.Local {
			n.MetadataGen = s.SettingInt("generation")
		}
		if n.Local {
			n.Address = ""
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// siteCapacity sums capacity and free space over online nodes.
func (s *Site) siteCapacity() (capacity, free int64) {
	s.DB.QueryRow(`SELECT COALESCE(SUM(capacity), 0), COALESCE(SUM(free), 0) FROM nodes WHERE last_seen >= ?`,
		time.Now().Add(-nodeTimeout).Unix()).Scan(&capacity, &free)
	return
}

// pickNode chooses an online node for a new block, weighted by free space.
func (s *Site) pickNode(size int64) (string, blockstore.Store, error) {
	rows, err := s.DB.Query(`SELECT id, free FROM nodes WHERE last_seen >= ? AND free > ?`,
		time.Now().Add(-nodeTimeout).Unix(), size)
	if err != nil {
		return "", nil, err
	}
	type cand struct {
		id   string
		free int64
	}
	var cands []cand
	var total float64
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.free); err != nil {
			rows.Close()
			return "", nil, err
		}
		if s.nodeStore(c.id) != nil {
			cands = append(cands, c)
			total += float64(c.free)
		}
	}
	rows.Close()
	if len(cands) == 0 {
		return "", nil, ErrNoSpace
	}
	r := rand.Float64() * total
	for _, c := range cands {
		r -= float64(c.free)
		if r <= 0 {
			return c.id, s.nodeStore(c.id), nil
		}
	}
	c := cands[len(cands)-1]
	return c.id, s.nodeStore(c.id), nil
}

// putOnNode stores data on a chosen node. The space is taken from the node's
// free space before writing, in the same statement that checks it, so blocks
// written at the same time (the parts of a large upload) can't together go
// past the node's storage limit. It is given back if the write fails.
func (s *Site) putOnNode(ctx context.Context, ns, id string, data []byte) (string, error) {
	size := int64(len(data))
	for range 5 { // another write may take the space first; pick again
		nodeID, store, err := s.pickNode(size)
		if err != nil {
			return "", err
		}
		res, err := s.DB.Exec(`UPDATE nodes SET free = free - ? WHERE id = ? AND free > ?`, size, nodeID, size)
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if err := store.Put(ctx, ns, id, data); err != nil {
			s.DB.Exec(`UPDATE nodes SET free = free + ? WHERE id = ?`, size, nodeID)
			return "", err
		}
		return nodeID, nil
	}
	return "", ErrNoSpace
}

// NodeJoinToken returns the token for adding storage nodes (the manager's
// key and the join secret) and the manager address nodes should use
// (IBS_MANAGER): the configured address, or else fallbackHost with the
// manager's TLS port.
func (s *Site) NodeJoinToken(fallbackHost string) (token, address string) {
	address = s.Setting("node_address")
	if address == "" && fallbackHost != "" {
		address = net.JoinHostPort(fallbackHost, s.peerPort())
	}
	return node.Token{Key: s.ID.Pub, Secret: s.Setting("join_token")}.Encode(), address
}

func (s *Site) peerPort() string {
	_, port, err := net.SplitHostPort(s.cfg.PeerAddr)
	if err != nil || port == "" {
		return "7443"
	}
	return port
}

// SetNodeAddress sets the host:port nodes use to reach this manager's TLS
// listener ("" derives it from the address the admin UI is opened with).
func (s *Site) SetNodeAddress(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr != "" {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, s.peerPort())
		}
	}
	if err := s.SetSetting("node_address", addr); err != nil {
		return err
	}
	s.writeJoinTokenFile()
	return nil
}

// RotateJoinToken replaces the join secret. Nodes that already joined keep
// working: they are recognized by their key.
func (s *Site) RotateJoinToken() error {
	if err := s.SetSetting("join_token", randomToken()); err != nil {
		return err
	}
	s.writeJoinTokenFile()
	return nil
}

// writeJoinTokenFile keeps the IBS_JOIN_TOKEN_OUT file current (when set and
// the node address is known), so automated deployments can hand the token
// to nodes without sharing the manager's data directory.
func (s *Site) writeJoinTokenFile() {
	p := s.cfg.JoinTokenOut
	if p == "" {
		return
	}
	token, _ := s.NodeJoinToken("")
	if token == "" {
		os.Remove(p)
		return
	}
	if err := os.WriteFile(p, []byte(token+"\n"), 0o644); err != nil {
		s.log.Warn("write join token file", "err", err)
	}
}
