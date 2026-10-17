package site

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ibs/internal/db"
	"ibs/internal/keys"
)

// ---- S3 access keys ----

// Access an access key gives: full (read and write) or read only.
const (
	AccessFull = "full"
	AccessRead = "read"
)

type AccessKey struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Access  string    `json:"access"`
	// Buckets the key may use; nil: all buckets, also ones created later.
	Buckets []string `json:"buckets"`
	Secret  string   `json:"secret,omitempty"` // only returned on creation
}

// KeyPolicy is what a key may do.
type KeyPolicy struct {
	Access  string
	Buckets map[string]bool // nil: all buckets
}

// CanRead reports whether the key may read bucket ("" = the bucket list).
func (p KeyPolicy) CanRead(bucket string) bool {
	return p.Buckets == nil || bucket == "" || p.Buckets[bucket]
}

// CanWrite reports whether the key may change bucket: create, upload,
// delete. Creating a bucket needs a key for all buckets, or one listing it.
func (p KeyPolicy) CanWrite(bucket string) bool {
	return p.Access == AccessFull && (p.Buckets == nil || p.Buckets[bucket])
}

func randomString(alphabet string, n int) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// checkKey validates a key's access and buckets and returns how to store
// the buckets.
func (s *Site) checkKey(access string, buckets []string) (string, error) {
	if access != AccessFull && access != AccessRead {
		return "", errors.New("access must be full or read")
	}
	if buckets == nil {
		return "", nil
	}
	if len(buckets) == 0 {
		return "", errors.New("choose at least one bucket, or all buckets")
	}
	for _, b := range buckets {
		if !s.BucketExists(b) {
			return "", fmt.Errorf("no bucket %q", b)
		}
	}
	slices.Sort(buckets)
	j, _ := json.Marshal(slices.Compact(buckets))
	return string(j), nil
}

func (s *Site) CreateAccessKey(name, access string, buckets []string) (*AccessKey, error) {
	stored, err := s.checkKey(access, buckets)
	if err != nil {
		return nil, err
	}
	k := &AccessKey{
		ID:      "IBS" + randomString("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 17),
		Name:    strings.TrimSpace(name),
		Created: time.Now().UTC(),
		Access:  access,
		Buckets: buckets,
		Secret:  randomString("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", 40),
	}
	enc := keys.Seal(s.ID.Master, []byte(k.Secret), []byte("access-key:"+k.ID))
	if _, err := s.DB.Exec(`INSERT INTO access_keys(id, secret_enc, name, created, access, buckets) VALUES(?, ?, ?, ?, ?, ?)`,
		k.ID, enc, k.Name, k.Created.Unix(), access, stored); err != nil {
		return nil, err
	}
	s.markDirty()
	return k, nil
}

// UpdateAccessKey changes a key's description, access and buckets; its
// secret stays.
func (s *Site) UpdateAccessKey(id, name, access string, buckets []string) error {
	stored, err := s.checkKey(access, buckets)
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(`UPDATE access_keys SET name = ?, access = ?, buckets = ? WHERE id = ?`, strings.TrimSpace(name), access, stored, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such access key")
	}
	s.markDirty()
	return nil
}

func decodeKeyBuckets(stored string) []string {
	if stored == "" {
		return nil
	}
	out := []string{}
	json.Unmarshal([]byte(stored), &out)
	return out
}

func (s *Site) AccessKeys() ([]AccessKey, error) {
	rows, err := s.DB.Query(`SELECT id, name, created, access, buckets FROM access_keys ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessKey
	for rows.Next() {
		var k AccessKey
		var c int64
		var buckets string
		if err := rows.Scan(&k.ID, &k.Name, &c, &k.Access, &buckets); err != nil {
			return nil, err
		}
		k.Created = time.Unix(c, 0).UTC()
		k.Buckets = decodeKeyBuckets(buckets)
		out = append(out, k)
	}
	return out, rows.Err()
}

// AccessKeyPolicy returns what a key may do.
func (s *Site) AccessKeyPolicy(id string) (KeyPolicy, bool) {
	var p KeyPolicy
	var buckets string
	if s.DB.QueryRow(`SELECT access, buckets FROM access_keys WHERE id = ?`, id).Scan(&p.Access, &buckets) != nil {
		return p, false
	}
	if list := decodeKeyBuckets(buckets); list != nil {
		p.Buckets = map[string]bool{}
		for _, b := range list {
			p.Buckets[b] = true
		}
	}
	return p, true
}

// forgetBucketInKeys removes a deleted bucket from the keys that list it, so
// a new bucket with the same name doesn't inherit their access.
func (s *Site) forgetBucketInKeys(bucket string) {
	rows, err := s.DB.Query(`SELECT id, buckets FROM access_keys WHERE buckets != ''`)
	if err != nil {
		return
	}
	type change struct{ id, buckets string }
	var changes []change
	for rows.Next() {
		var id, stored string
		rows.Scan(&id, &stored)
		list := decodeKeyBuckets(stored)
		if i := slices.Index(list, bucket); i >= 0 {
			j, _ := json.Marshal(slices.Delete(list, i, i+1))
			changes = append(changes, change{id, string(j)})
		}
	}
	rows.Close()
	for _, c := range changes {
		s.DB.Exec(`UPDATE access_keys SET buckets = ? WHERE id = ?`, c.buckets, c.id)
	}
}

func (s *Site) DeleteAccessKey(id string) error {
	_, err := s.DB.Exec(`DELETE FROM access_keys WHERE id = ?`, id)
	s.accessKeys.Delete(id)
	s.markDirty()
	return err
}

// AccessKeySecret returns the secret for SigV4 verification.
func (s *Site) AccessKeySecret(id string) (string, bool) {
	if v, ok := s.accessKeys.Load(id); ok {
		return v.(string), true
	}
	var enc []byte
	if err := s.DB.QueryRow(`SELECT secret_enc FROM access_keys WHERE id = ?`, id).Scan(&enc); err != nil {
		return "", false
	}
	secret, err := keys.Open(s.ID.Master, enc, []byte("access-key:"+id))
	if err != nil {
		return "", false
	}
	s.accessKeys.Store(id, string(secret))
	return string(secret), true
}

// ---- Settings ----

type Settings struct {
	SiteName      string `json:"site_name"`
	PeerAdvertise string `json:"peer_advertise"`
}

func (s *Site) Settings() Settings {
	return Settings{
		SiteName:      s.Name(),
		PeerAdvertise: s.Setting("peer_advertise"),
	}
}

func (s *Site) UpdateSettings(in Settings) error {
	if strings.TrimSpace(in.SiteName) == "" {
		return errors.New("site name is required")
	}
	for k, v := range map[string]string{
		"site_name":      strings.TrimSpace(in.SiteName),
		"peer_advertise": strings.TrimSpace(in.PeerAdvertise),
	} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Site) ChangePassword(oldPw, newPw string) error {
	if !s.CheckAdminPassword(oldPw) {
		return errors.New("current password is incorrect")
	}
	if len(newPw) < 8 {
		return errors.New("new password must be at least 8 characters")
	}
	return s.SetSetting("admin_password", keys.HashPassword(newPw))
}

// SessionKey signs admin session cookies.
func (s *Site) SessionKey() []byte {
	k := s.Setting("session_key")
	if k == "" {
		k = keys.RandomToken(32)
		s.SetSetting("session_key", k)
	}
	return []byte(k + s.Setting("admin_password"))
}

// ---- Dashboard ----

type Dashboard struct {
	Site struct {
		SiteID        string `json:"site_id"`
		Name          string `json:"name"`
		UpSince       int64  `json:"up_since"`
		Capacity      int64  `json:"capacity"`
		Free          int64  `json:"free"`
		Offered       int64  `json:"offered"` // free space offered to other sites' copies
		Generation    int64  `json:"generation"`
		PeerAdvertise string `json:"peer_advertise"`
		WebHost       string `json:"web_host,omitempty"` // guessed LAN host (NoteWebHost)
		Buckets       int64  `json:"buckets"`
		Objects       int64  `json:"objects"`
		ObjectBytes   int64  `json:"object_bytes"`
		HeldBlocks    int64  `json:"held_blocks"`
		HeldBytes     int64  `json:"held_bytes"`
		// Of those, copies of sites that are offline: they can't make way.
		HeldOffline int64     `json:"held_offline"`
		Location    *Location `json:"location,omitempty"`
		Restored    int64     `json:"restored"`
	} `json:"site"`
	// Counted: the figures from the cache are in; false right after a start,
	// before the first calculation.
	Counted     bool              `json:"counted"`
	Replication ReplicationHealth `json:"replication"`
	Capacity    IBSCapacity       `json:"ibs_capacity"`
	Nodes       []NodeInfo        `json:"nodes"`
	Peers       []PeerInfo        `json:"peers"`
	Network     []NetSite         `json:"network"`
	Relays      []RelayInfo       `json:"relays"`
	Links       []Link            `json:"links"`
	RelayNodes  []RelayNode       `json:"relay_nodes"`
}

func (s *Site) Dashboard() (*Dashboard, error) {
	d := &Dashboard{}
	d.Site.SiteID = s.ID.SiteID
	d.Site.Name = s.Name()
	d.Site.UpSince = s.started.Unix()
	d.Site.Capacity, d.Site.Free = s.siteCapacity()
	_, d.Site.Offered = s.offeredStorage()
	d.Site.Generation = s.SettingInt("generation")
	d.Site.PeerAdvertise = s.Setting("peer_advertise")
	d.Site.WebHost = s.WebHost()
	d.Site.Restored = s.SettingInt("restored")
	d.Site.Location = s.Location()
	s.DB.QueryRow(`SELECT COUNT(*) FROM buckets`).Scan(&d.Site.Buckets)
	// The expensive figures come from the cache (stats.go).
	if st := s.stats(); st != nil {
		d.Counted = true
		d.Site.Objects, d.Site.ObjectBytes = st.Objects, st.ObjectBytes
		d.Site.HeldBlocks, d.Site.HeldBytes, d.Site.HeldOffline = st.HeldBlocks, st.HeldBytes, st.HeldOffline
		d.Replication, d.Capacity = st.Health, st.Capacity
	}
	var err error
	if d.Nodes, err = s.Nodes(); err != nil {
		return nil, err
	}
	if d.Peers, err = s.Peers(); err != nil {
		return nil, err
	}
	if d.Network, err = s.Network(); err != nil {
		return nil, err
	}
	// Encode empty lists as [] rather than null for the UI.
	d.Nodes = nonNil(d.Nodes)
	d.Peers = nonNil(d.Peers)
	d.Network = nonNil(d.Network)
	d.Relays = s.Relays()
	d.Links = nonNil(s.NetworkLinks())
	d.RelayNodes = relayNodes(d.Links)
	return d, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// IBSCapacity answers whether this site's replication fits on the IBS as a
// whole: enough distinct online sites for the highest replica count, and
// enough combined storage offered by them for the copies still to be made.
type IBSCapacity struct {
	Sites  int   `json:"sites"`
	Free   int64 `json:"free"`
	Needed int64 `json:"needed"`
	Copies int   `json:"copies"` // copies each file should have on other sites
	Fits   bool  `json:"fits"`
}

func (s *Site) IBSCapacity(h ReplicationHealth) IBSCapacity {
	c := IBSCapacity{Needed: h.PendingBytes, Copies: Copies}
	var free []int64
	for _, p := range s.onlinePeers() {
		c.Sites++
		c.Free += p.free
		free = append(free, max(p.free, 0))
	}
	// Blocks missing both copies need two different sites; the others one.
	both, one := h.noCopy, h.someCopies
	c.Fits = c.Sites >= Copies && one+Copies*both <= c.Free && both <= replicable(free, Copies)
	return c
}

// StorableCapacity estimates how much more data this site can store right
// now with its copies on other sites: one copy here (within the nodes'
// limits) and one on each of Copies different online sites, using the space
// they offer. A site holds at most one copy of a block, so the replicated
// amount X is the largest value with sum(min(free_i, X)) >= Copies*X: one
// large site cannot stand in for two. Other sites may count on the same free
// space, so it is an estimate.
type StorableCapacity struct {
	Local     int64  `json:"local"`      // free space at this site
	Sites     int    `json:"sites"`      // other sites online
	Copies    int    `json:"copies"`     // copies on other sites files get now (at most Copies)
	Storable  int64  `json:"storable"`   // how much more can be stored
	LimitedBy string `json:"limited_by"` // "site", "ibs" (other sites' space) or "sites" (too few sites)
}

func (s *Site) StorableCapacity() StorableCapacity {
	_, local := s.siteCapacity()
	var free []int64
	for _, p := range s.onlinePeers() {
		free = append(free, max(p.free, 0))
	}
	c := StorableCapacity{Local: local, Sites: len(free), Copies: min(Copies, len(free))}
	if c.Copies == 0 {
		c.Storable, c.LimitedBy = local, "sites"
		return c
	}
	if ibs := replicable(free, c.Copies); local <= ibs {
		c.Storable, c.LimitedBy = local, "site"
	} else {
		c.Storable, c.LimitedBy = ibs, "ibs"
	}
	if c.Copies < Copies {
		c.LimitedBy = "sites"
	}
	return c
}

// replicable returns the largest X with sum(min(free_i, X)) >= r*X, i.e. how
// much data can be placed with r copies on distinct sites.
func replicable(free []int64, r int) int64 {
	var total int64
	for _, f := range free {
		total += f
	}
	lo, hi := int64(0), total/int64(r)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		var sum int64
		for _, f := range free {
			sum += min(f, mid)
		}
		if sum >= int64(r)*mid {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// RecoveryPhrase reads the recovery phrase of the site in dataDir. It works
// without starting the site; it needs access to the data directory.
func RecoveryPhrase(dataDir string) (string, error) {
	seed, err := os.ReadFile(filepath.Join(dataDir, identityFile))
	if err != nil {
		return "", errors.New("no site in " + dataDir)
	}
	return keys.EncodePhrase(seed), nil
}

// ResetPassword sets a new admin password for the site in dataDir (for when
// it is forgotten). It needs access to the data directory.
func ResetPassword(dataDir, password string) error {
	if !IsSetUp(dataDir) {
		return errors.New("no site in " + dataDir)
	}
	if len(password) < 8 {
		return errors.New("the password must be at least 8 characters")
	}
	d, err := db.Open(filepath.Join(dataDir, dbFile))
	if err != nil {
		return err
	}
	defer d.Close()
	return db.SetSetting(d, "admin_password", keys.HashPassword(password))
}
