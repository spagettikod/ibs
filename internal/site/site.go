// Package site implements an IBS site manager: bucket and object metadata,
// block placement on local nodes, replication to peer sites, reconciliation
// and recovery.
package site

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"ibs/internal/blockstore"
	"ibs/internal/config"
	"ibs/internal/db"
	"ibs/internal/keys"
)

const (
	identityFile = "identity.seed"
	dbFile       = "ibs.db"
	LocalNodeID  = "local"
)

type Site struct {
	cfg     config.Config
	ID      *keys.Identity
	DB      *sql.DB
	log     *slog.Logger
	started time.Time

	nodesMu sync.RWMutex
	nodes   map[string]blockstore.Store

	blockLocks [256]sync.Mutex

	peerMu      sync.Mutex
	peerClients map[string]*peerClient
	pushedGen   map[string]int64 // site id -> backup generation last pushed
	nodeMetaGen map[string]int64 // node id -> metadata generation last pushed
	reconciling map[string]bool

	bucketKeys sync.Map // bucket name -> []byte
	accessKeys sync.Map // access key id -> secret

	dirty     atomic.Bool
	reconcile chan string

	events    *hub
	lastState string // fingerprint used by watchState

	// Networking: relays, relayed connections, direct QUIC.
	netMu   sync.Mutex
	runCtx  context.Context
	quicTr  *quic.Transport
	relays  map[string]*relayEntry
	relayed *connQueue
	webHost string // last host the web interface was opened with (NoteWebHost)
	// relayMoveTried: when a relay was last tried at an address another site uses.
	relayMoveTried map[string]time.Time

	linksMu sync.Mutex
	links   map[string]Link // learned from other sites
	updates updateState
	scrub   scrubState
	// scrubLimit keeps scrub reads to IBS_SCRUB_RATE: this site's scrubs and
	// the checks other sites' scrubs ask for.
	scrubLimit byteLimiter
	// rebuildLimit keeps re-creating copies of a gone site to IBS_REBUILD_RATE.
	rebuildLimit byteLimiter
	roomMu       sync.Mutex   // one makeRoom at a time
	noRoom       atomic.Int64 // blocks no site had room for at the last replication run
	// replCursor: the last block of the previous replication batch (replicateOnce only).
	replCursor string
	// activeUploads: multipart uploads with a part arriving now (uploadActive).
	activeMu      sync.Mutex
	activeUploads map[string]int
	replNoRoom    int64    // blocks without room so far in this pass (replicateOnce only)
	rates         sync.Map // site id -> *peerRates: copy traffic (rates.go)
	wasMoving     bool     // copies were moving at the last transferEvents
}

// IsSetUp reports whether the data directory holds an initialized site.
func IsSetUp(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, identityFile))
	return err == nil
}

// Create initializes a brand new site and returns its recovery phrase.
func Create(cfg config.Config, name, password string) (*Site, string, error) {
	if IsSetUp(cfg.DataDir) {
		return nil, "", errors.New("site is already set up")
	}
	if name == "" || len(password) < 8 {
		return nil, "", errors.New("site name and an admin password of at least 8 characters are required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, "", err
	}
	seed := keys.NewSeed()
	d, err := db.Open(filepath.Join(cfg.DataDir, dbFile))
	if err != nil {
		return nil, "", err
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	for k, v := range map[string]string{
		"site_name":      name,
		"admin_password": keys.HashPassword(password),
		"created":        now,
		"generation":     "0",
		"join_token":     randomToken(),
		"peer_advertise": cfg.PeerAdvertise,
	} {
		if err := db.SetSetting(d, k, v); err != nil {
			d.Close()
			return nil, "", err
		}
	}
	d.Close()
	if err := os.WriteFile(filepath.Join(cfg.DataDir, identityFile), seed, 0o600); err != nil {
		return nil, "", err
	}
	s, err := Open(cfg)
	if err != nil {
		return nil, "", err
	}
	return s, keys.EncodePhrase(seed), nil
}

// Open loads an existing site from the data directory.
func Open(cfg config.Config) (*Site, error) {
	seed, err := os.ReadFile(filepath.Join(cfg.DataDir, identityFile))
	if err != nil {
		return nil, fmt.Errorf("read identity: %w", err)
	}
	id, err := keys.FromSeed(seed)
	if err != nil {
		return nil, err
	}
	d, err := db.Open(filepath.Join(cfg.DataDir, dbFile))
	if err != nil {
		return nil, err
	}
	s := &Site{
		cfg:            cfg,
		ID:             id,
		DB:             d,
		log:            slog.Default().With("site", id.SiteID[:8]),
		started:        time.Now(),
		nodes:          map[string]blockstore.Store{},
		peerClients:    map[string]*peerClient{},
		pushedGen:      map[string]int64{},
		nodeMetaGen:    map[string]int64{},
		reconciling:    map[string]bool{},
		reconcile:      make(chan string, 64),
		scrub:          scrubState{wake: make(chan struct{}, 1)},
		relayMoveTried: map[string]time.Time{},
		events:         newHub(),
		relays:         map[string]*relayEntry{},
		relayed:        newConnQueue(),
		links:          map[string]Link{},
	}
	local := &blockstore.FS{Dir: filepath.Join(cfg.DataDir, "blocks")}
	s.nodes[LocalNodeID] = local
	_, err = d.Exec(`INSERT INTO nodes(id, name, address, secret, last_seen, is_local)
		VALUES(?, 'manager', 'local', '', ?, 1) ON CONFLICT(id) DO UPDATE SET last_seen = excluded.last_seen`,
		LocalNodeID, time.Now().Unix())
	if err != nil {
		d.Close()
		return nil, err
	}
	if cfg.PeerAdvertise != "" {
		db.SetSetting(d, "peer_advertise", cfg.PeerAdvertise)
	}
	if cfg.NodeAddress != "" {
		db.SetSetting(d, "node_address", cfg.NodeAddress)
	}
	if err := s.loadNodes(); err != nil {
		d.Close()
		return nil, err
	}
	s.refreshLocalNode(context.Background())
	s.writeJoinTokenFile()
	s.noteDBLevel()
	// No upload survives a restart: blocks of ones that were in progress are
	// released, and the garbage collector removes them.
	d.Exec(`UPDATE blocks SET upload = '' WHERE upload != ''`)
	return s, nil
}

func (s *Site) Close() error { return s.DB.Close() }

func (s *Site) Setting(k string) string { return db.GetSetting(s.DB, k) }

// TLSBackup is the manager's HTTPS state kept in the metadata, so it is
// backed up and restored with the site.
func (s *Site) TLSBackup() []byte { return []byte(s.Setting("tls_backup")) }

// SetTLSBackup stores the manager's HTTPS state when it changed.
func (s *Site) SetTLSBackup(b []byte) error {
	if string(b) == s.Setting("tls_backup") {
		return nil
	}
	if err := db.SetSetting(s.DB, "tls_backup", string(b)); err != nil {
		return err
	}
	s.markDirty()
	return nil
}

func (s *Site) SettingInt(k string) int64 {
	n, _ := strconv.ParseInt(s.Setting(k), 10, 64)
	return n
}

func (s *Site) SetSetting(k, v string) error {
	s.markDirty()
	return db.SetSetting(s.DB, k, v)
}

func (s *Site) Name() string { return s.Setting("site_name") }

func (s *Site) StartedAt() time.Time { return s.started }

func (s *Site) CheckAdminPassword(pw string) bool {
	return keys.CheckPassword(s.Setting("admin_password"), pw)
}

func randomToken() string { return keys.RandomToken(24) }

func (s *Site) markDirty() {
	s.dirty.Store(true)
	s.notify(TopicData)
}

func (s *Site) lockBlock(id string) func() {
	var idx byte
	if len(id) > 0 {
		idx = id[0] ^ id[len(id)-1]
	}
	m := &s.blockLocks[idx]
	m.Lock()
	return m.Unlock
}

// Run starts the background loops: node and peer health, replication,
// garbage collection, healing, scrubbing and metadata backup.
func (s *Site) Run(ctx context.Context) {
	every := func(d time.Duration, name string, f func(context.Context)) {
		go func() {
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				func() {
					defer func() {
						if r := recover(); r != nil {
							s.log.Error("loop panicked", "loop", name, "panic", r)
						}
					}()
					f(ctx)
				}()
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	iv := s.cfg.CheckInterval
	every(iv, "nodes", s.refreshLocalNode)
	every(time.Second, "watch", s.watchState)
	every(iv, "peers", s.checkPeers)
	// Replication: the next batch at once while there is work, else after iv.
	go func() {
		for ctx.Err() == nil {
			more := false
			func() {
				defer func() {
					if r := recover(); r != nil {
						s.log.Error("loop panicked", "loop", "replicate", "panic", r)
					}
				}()
				more = s.replicateOnce(ctx)
			}()
			if !more {
				select {
				case <-ctx.Done():
				case <-time.After(iv):
				}
			}
		}
	}()
	every(2*time.Second, "transfer-events", s.transferEvents)
	every(iv, "trim", s.trimOnce)
	every(iv, "gc", s.collectGarbage)
	every(iv, "heal", s.healOnce)
	every(iv, "drain", s.drainOnce)
	every(12*iv, "releases", s.fetchReleases) // a minute by default
	every(3*iv, "backup", s.backupOnce)
	go s.scrubLoop(ctx)
	every(30*time.Minute, "reconcile-all", s.reconcileAll)
	every(6*iv, "relay-moves", s.followRelayMoves)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case id := <-s.reconcile:
				go s.reconcilePeer(ctx, id)
			}
		}
	}()
}
