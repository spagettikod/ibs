// Package config reads process configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Mode    string // "manager" or "node"
	DataDir string

	// Manager
	WebAddr       string // admin UI + API
	S3Addr        string // S3 endpoint
	PeerAddr      string // inter-site TLS listener
	PeerAdvertise string // address other sites use to reach this site's peer listener
	S3Domain      string // optional, enables virtual-host style bucket addressing
	SiteName      string // optional, non-interactive setup
	AdminPassword string // optional, non-interactive setup

	// HTTPS for the web interface and S3, with a certificate from an ACME CA
	// (Let's Encrypt) through the DNS-01 challenge.
	TLSDomains      []string // IBS_TLS_DOMAINS; empty = plain HTTP
	TLSDNSProvider  string   // lego DNS provider code
	TLSEmail        string
	TLSCA           string
	TLSDNSResolvers []string
	TLSDNSWait      string // wait instead of checking that TXT records are visible, e.g. "2m"
	TLSRequire      bool   // refuse plain HTTP once the certificate is in place

	PeerGrace     time.Duration // offline time before a peer's replicas stop counting
	RoomCooldown  time.Duration // after making room for its own files, how long a site takes no copies
	CheckInterval time.Duration // peer health / replication loop interval

	QUICAddr       string // manager: UDP address for direct site-to-site connections
	RelayAddress   string // manager: address of the relay given by IBS_RELAY_TOKEN(_FILE)
	RelayToken     string // manager: register with this relay on start
	RelayTokenFile string // manager: register with the relay whose token is in this file
	NodeAddress    string // manager: host:port storage nodes use to reach the TLS listener
	JoinTokenOut   string // manager: file to keep the current node join token in (automation)

	// Storage node
	Manager       string // manager address, host:port of its TLS listener (IBS_MANAGER)
	JoinToken     string
	JoinTokenFile string
	NodeAddr      string
	NodeAdvert    string
	NodeName      string

	// Any node, including the manager: how much of the disk IBS may use for
	// this site's data and blocks held for other sites. Unset (0) = the whole
	// disk; NoStorage (IBS_STORAGE_LIMIT=0) = share nothing.
	StorageLimit int64
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	rawLimit := env("IBS_STORAGE_LIMIT", "")
	limit, err := ParseSize(rawLimit)
	if err != nil {
		return Config{}, fmt.Errorf("IBS_STORAGE_LIMIT: %w", err)
	}
	if rawLimit != "" && limit == 0 {
		limit = NoStorage // an explicit 0: this node stores no blocks
	}
	return Config{
		StorageLimit:    limit,
		Mode:            env("IBS_MODE", "manager"),
		DataDir:         env("IBS_DATA", "/data"),
		WebAddr:         env("IBS_WEB_ADDR", ":8080"),
		S3Addr:          env("IBS_S3_ADDR", ":9000"),
		PeerAddr:        env("IBS_PEER_ADDR", ":7443"),
		PeerAdvertise:   env("IBS_PEER_ADVERTISE", ""),
		S3Domain:        env("IBS_S3_DOMAIN", ""),
		SiteName:        env("IBS_SITE_NAME", ""),
		AdminPassword:   env("IBS_ADMIN_PASSWORD", ""),
		TLSDomains:      list("IBS_TLS_DOMAINS"),
		TLSDNSProvider:  env("IBS_TLS_DNS_PROVIDER", ""),
		TLSEmail:        env("IBS_TLS_EMAIL", ""),
		TLSCA:           env("IBS_TLS_CA", ""),
		TLSDNSResolvers: list("IBS_TLS_DNS_RESOLVERS"),
		TLSDNSWait:      env("IBS_TLS_DNS_PROPAGATION_WAIT", ""),
		TLSRequire:      env("IBS_TLS_REQUIRE", "true") != "false",
		PeerGrace:       dur("IBS_PEER_GRACE", 30*time.Minute),
		RoomCooldown:    dur("IBS_ROOM_COOLDOWN", time.Hour),
		CheckInterval:   dur("IBS_CHECK_INTERVAL", 5*time.Second),
		QUICAddr:        env("IBS_QUIC_ADDR", ":7443"),
		RelayAddress:    env("IBS_RELAY_ADDRESS", ""),
		RelayToken:      env("IBS_RELAY_TOKEN", ""),
		RelayTokenFile:  env("IBS_RELAY_TOKEN_FILE", ""),
		NodeAddress:     env("IBS_NODE_JOIN_ADDRESS", ""),
		JoinTokenOut:    env("IBS_JOIN_TOKEN_OUT", ""),
		Manager:         env("IBS_MANAGER", ""),
		JoinToken:       env("IBS_JOIN_TOKEN", ""),
		JoinTokenFile:   env("IBS_JOIN_TOKEN_FILE", ""),
		NodeAddr:        env("IBS_NODE_ADDR", ":7480"),
		NodeAdvert:      env("IBS_NODE_ADVERTISE", ""),
		NodeName:        env("IBS_NODE_NAME", hostname()),
	}, nil
}

// NoStorage is the storage limit of a node that shares no disk space, e.g. a
// manager that only keeps metadata and leaves all data to storage nodes.
const NoStorage = -1

// ParseSize parses a byte size: plain bytes or a number with a K, M, G, T or
// P suffix (binary units, optional "B"/"iB"), e.g. "500G", "1.5T", "750GiB".
// "" means 0.
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return 0, nil
	}
	t = strings.TrimSuffix(strings.TrimSuffix(t, "B"), "I")
	mult := float64(1)
	if n := len(t); n > 0 {
		if i := strings.IndexByte("KMGTP", t[n-1]); i >= 0 {
			mult = float64(int64(1) << (10 * (i + 1)))
			t = t[:n-1]
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q (use bytes or a value like 500G)", s)
	}
	return int64(f * mult), nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func dur(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil && d > 0 {
		return d
	}
	return def
}

// list reads a comma- or space-separated list.
func list(k string) []string {
	return strings.FieldsFunc(os.Getenv(k), func(r rune) bool { return r == ',' || r == ' ' })
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
