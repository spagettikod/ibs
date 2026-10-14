package node

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"ibs/internal/config"
	"ibs/internal/keys"
	"ibs/internal/release"
)

// FetchRelease gets the version of IBS the manager runs, for a node's first
// start: the launcher calls it, so a node needs nothing from GitHub and no
// release settings of its own. It returns the release and the keys the
// manager trusts releases by. The manager is found, and pinned by its key,
// the same way the node finds it; the node proves it may join with the join
// secret, or with its own key once it has joined.
func FetchRelease(ctx context.Context, cfg config.Config, tokenArg string) (release.Bundle, []string, error) {
	st, err := loadState(filepath.Join(cfg.DataDir, "node.json"))
	if err != nil {
		return release.Bundle{}, nil, err
	}
	addr, key, secret := withDefaultPort(cfg.Manager, "7443"), ed25519.PublicKey(st.ManagerKey), st.Secret
	if raw, _ := readToken(tokenArg, cfg); raw != "" {
		t, err := ParseToken(raw)
		if err != nil {
			return release.Bundle{}, nil, err
		}
		key, secret = t.Key, t.Secret
		if addr == "" {
			addr = t.Address
		}
	}
	if addr == "" {
		addr = st.Manager
	}
	if addr == "" || len(key) != ed25519.PublicKeySize {
		return release.Bundle{}, nil, errors.New("no manager: set IBS_MANAGER and give the join token")
	}

	// The node's own key: the manager knows it once the node has joined. Before
	// that a new key does, since only the secret matters then; the node
	// creates and keeps its key when IBS starts.
	cert, err := keys.SelfSignedCert(ed25519.NewKeyFromSeed(st.Seed), "ibs-node-"+st.ID)
	if err != nil {
		return release.Bundle{}, nil, err
	}
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: keys.ClientTLS(cert, key)},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/cluster/v1/release", nil)
	if secret != "" {
		req.Header.Set("X-IBS-Join-Secret", secret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return release.Bundle{}, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if strings.TrimSpace(string(msg)) == "404 page not found" {
			return release.Bundle{}, nil, fmt.Errorf("manager %s runs an IBS version that can't give nodes IBS; update it to 1.3.0 or newer", addr)
		}
		return release.Bundle{}, nil, fmt.Errorf("manager %s: %s", addr, strings.TrimSpace(string(msg)))
	}
	b, err := release.ReadTar(resp.Body)
	if err != nil {
		return release.Bundle{}, nil, err
	}
	var trusted []string
	for k := range strings.SplitSeq(resp.Header.Get("X-IBS-Trusted-Keys"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			trusted = append(trusted, k)
		}
	}
	return b, trusted, nil
}
