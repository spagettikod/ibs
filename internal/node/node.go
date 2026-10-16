package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ibs/internal/blockstore"
	"ibs/internal/config"
	"ibs/internal/keys"
	"ibs/internal/release"
	"ibs/internal/stall"
	"ibs/internal/version"
)

// state is persisted in <data>/node.json so a node keeps its identity and
// remembers its manager across restarts without the join token.
type state struct {
	ID         string `json:"id"`
	Seed       []byte `json:"seed"`
	Manager    string `json:"manager,omitempty"`
	ManagerKey []byte `json:"manager_key,omitempty"`
	Secret     string `json:"secret,omitempty"` // join secret, kept until the first registration succeeds
	Joined     bool   `json:"joined,omitempty"`
}

func loadState(path string) (*state, error) {
	st := &state{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, st); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	if len(st.Seed) != ed25519.SeedSize {
		st.Seed = make([]byte, ed25519.SeedSize)
		rand.Read(st.Seed)
	}
	if st.ID == "" {
		h := sha256.Sum256(ed25519.NewKeyFromSeed(st.Seed).Public().(ed25519.PublicKey))
		st.ID = hex.EncodeToString(h[:8])
	}
	return st, nil
}

func (st *state) save(path string) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// withDefaultPort adds port to an address without one ("" stays "").
func withDefaultPort(addr, port string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, port)
	}
	return addr
}

// readToken finds a join token: the argument, IBS_JOIN_TOKEN, or the file
// named by IBS_JOIN_TOKEN_FILE (which may appear later, e.g. while the
// manager is still starting).
func readToken(arg string, cfg config.Config) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if cfg.JoinToken != "" {
		return cfg.JoinToken, nil
	}
	if cfg.JoinTokenFile != "" {
		b, err := os.ReadFile(cfg.JoinTokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

// Run starts a storage node. tokenArg is the join token from the command
// line, if any.
func Run(ctx context.Context, cfg config.Config, tokenArg string) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	statePath := filepath.Join(cfg.DataDir, "node.json")
	st, err := loadState(statePath)
	if err != nil {
		return err
	}

	// The manager's address comes from IBS_MANAGER, else from an older token
	// that includes it, else from the last start. A token (re)points the node
	// at a manager; without one the node rejoins the manager it knows.
	manager := withDefaultPort(cfg.Manager, "7443")
	for {
		raw, err := readToken(tokenArg, cfg)
		if err == nil && raw != "" {
			t, err := ParseToken(raw)
			if err != nil {
				return err
			}
			addr := manager
			if addr == "" {
				addr = st.Manager
			}
			if addr == "" {
				return errors.New("set IBS_MANAGER to the manager's address (host:7443), shown on its Nodes page")
			}
			if !bytes.Equal(t.Key, st.ManagerKey) || !st.Joined {
				st.ManagerKey, st.Secret, st.Joined = t.Key, t.Secret, false
			}
			st.Manager = addr
			break
		}
		if st.Manager != "" {
			if manager != "" {
				st.Manager = manager // the manager moved
			}
			break
		}
		if cfg.JoinTokenFile == "" {
			return errors.New("no join token: run `ibs node <token>` with the token from the manager's Nodes page")
		}
		slog.Info("waiting for join token file", "path", cfg.JoinTokenFile)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
	if err := st.save(statePath); err != nil {
		return err
	}

	priv := ed25519.NewKeyFromSeed(st.Seed)
	cert, err := keys.SelfSignedCert(priv, "ibs-node-"+st.ID)
	if err != nil {
		return err
	}
	managerKey := ed25519.PublicKey(st.ManagerKey)

	// Only the manager, identified by its key, may use the block API.
	store := &blockstore.FS{Dir: filepath.Join(cfg.DataDir, "blocks")}
	srv := &http.Server{
		Addr:              cfg.NodeAddr,
		Handler:           blockstore.Handler(store, &blockstore.MetaFile{Dir: cfg.DataDir}),
		TLSConfig:         keys.ServerTLS(cert, managerKey),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	go func() {
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("node listener stopped", "addr", cfg.NodeAddr, "err", err)
			os.Exit(1)
		}
	}()
	_, portStr, _ := net.SplitHostPort(cfg.NodeAddr)
	port, _ := strconv.Atoi(portStr)
	slog.Info("storage node running", "node", st.ID, "listen", cfg.NodeAddr, "manager", st.Manager, "storage_limit", cfg.StorageLimit)

	// Requests are limited by their progress (stall.Do), not by total time:
	// fetching a new version may take a while on a slow network.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: keys.ClientTLS(cert, managerKey),
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second}}
	register := func() error {
		capacity, free, err := store.Stat(ctx)
		if err != nil {
			return err
		}
		reg := Registration{ID: st.ID, Name: cfg.NodeName, Port: port, Advertise: cfg.NodeAdvert, Capacity: capacity, Free: free, Limit: cfg.StorageLimit,
			Version: version.Version}
		if !st.Joined {
			reg.Secret = st.Secret
		}
		body, _ := json.Marshal(reg)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+st.Manager+"/cluster/v1/register", bytes.NewReader(body))
		resp, err := stall.Do(client, req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return fmt.Errorf("manager rejected registration: %s", strings.TrimSpace(string(msg)))
		}
		var reply RegistrationReply
		json.NewDecoder(resp.Body).Decode(&reply)
		if reply.TargetVersion != "" && reply.TargetVersion != version.Version {
			if err := follow(ctx, client, cfg.DataDir, st.Manager, reply); err != nil {
				slog.Warn("update to the manager's version", "version", reply.TargetVersion, "err", err)
			} else {
				return ErrRestart
			}
		}
		if !st.Joined {
			st.Joined, st.Secret = true, ""
			if err := st.save(statePath); err != nil {
				return err
			}
			slog.Info("joined site", "site", reply.SiteName, "reachable_at", reply.Address)
		}
		return nil
	}

	failing := false
	t := time.NewTicker(max(3*cfg.CheckInterval, time.Second)) // 15s by default
	defer t.Stop()
	for {
		if err := register(); errors.Is(err, ErrRestart) {
			return err
		} else if err != nil {
			slog.Warn("register with manager", "manager", st.Manager, "err", err)
			failing = true
		} else if failing {
			slog.Info("manager reachable again")
			failing = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// ErrRestart asks for the node to exit so the launcher starts the version
// now marked current.
var ErrRestart = errors.New("restart into a new version")

// follow installs the manager's version on this node: fetched from the
// manager, checked against the trusted keys, and marked current. Only
// possible when the node was started by the launcher.
func follow(ctx context.Context, client *http.Client, dataDir, manager string, reply RegistrationReply) error {
	if os.Getenv("IBS_LAUNCHER") == "" { // set to its version by the launcher
		return errors.New("this node wasn't started by the IBS launcher; update its image to " + reply.TargetVersion)
	}
	store := release.Store{Dir: dataDir}
	for _, k := range reply.TrustedKeys {
		if _, err := store.AddKey(k, "from the manager"); err != nil {
			return err
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+manager+"/cluster/v1/releases/"+reply.TargetVersion, nil)
	resp, err := stall.Do(client, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("manager: %s", resp.Status)
	}
	b, err := release.ReadTar(resp.Body)
	if err != nil {
		return err
	}
	in, err := store.Install(b, store.Keys())
	if err != nil {
		return err
	}
	if in.Version != reply.TargetVersion {
		return fmt.Errorf("got version %s, wanted %s", in.Version, reply.TargetVersion)
	}
	if err := store.SetCurrent(in.Version); err != nil {
		return err
	}
	slog.Info("switching to the manager's version", "from", version.Version, "to", in.Version)
	return nil
}
