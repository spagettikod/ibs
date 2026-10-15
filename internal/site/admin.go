package site

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ibs/internal/db"
	"ibs/internal/keys"
)

// ---- S3 access keys ----

type AccessKey struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Secret  string    `json:"secret,omitempty"` // only returned on creation
}

func randomString(alphabet string, n int) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func (s *Site) CreateAccessKey(name string) (*AccessKey, error) {
	k := &AccessKey{
		ID:      "IBS" + randomString("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 17),
		Name:    strings.TrimSpace(name),
		Created: time.Now().UTC(),
		Secret:  randomString("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", 40),
	}
	enc := keys.Seal(s.ID.Master, []byte(k.Secret), []byte("access-key:"+k.ID))
	if _, err := s.DB.Exec(`INSERT INTO access_keys(id, secret_enc, name, created) VALUES(?, ?, ?, ?)`,
		k.ID, enc, k.Name, k.Created.Unix()); err != nil {
		return nil, err
	}
	s.markDirty()
	return k, nil
}

func (s *Site) AccessKeys() ([]AccessKey, error) {
	rows, err := s.DB.Query(`SELECT id, name, created FROM access_keys ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessKey
	for rows.Next() {
		var k AccessKey
		var c int64
		if err := rows.Scan(&k.ID, &k.Name, &c); err != nil {
			return nil, err
		}
		k.Created = time.Unix(c, 0).UTC()
		out = append(out, k)
	}
	return out, rows.Err()
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
	PeerShare     int    `json:"peer_share"` // percent of this site's storage other sites' copies may use
}

func (s *Site) Settings() Settings {
	return Settings{
		SiteName:      s.Name(),
		PeerAdvertise: s.Setting("peer_advertise"),
		PeerShare:     s.PeerShare(),
	}
}

func (s *Site) UpdateSettings(in Settings) error {
	if strings.TrimSpace(in.SiteName) == "" {
		return errors.New("site name is required")
	}
	if in.PeerShare == 0 {
		in.PeerShare = s.PeerShare() // not sent: unchanged
	}
	if in.PeerShare < 10 || in.PeerShare > 100 {
		return errors.New("other sites' copies may use between 10% and 100% of this site's storage")
	}
	for k, v := range map[string]string{
		"site_name":      strings.TrimSpace(in.SiteName),
		"peer_advertise": strings.TrimSpace(in.PeerAdvertise),
		"peer_share":     strconv.Itoa(in.PeerShare),
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
		SiteID        string    `json:"site_id"`
		Name          string    `json:"name"`
		UpSince       int64     `json:"up_since"`
		Capacity      int64     `json:"capacity"`
		Free          int64     `json:"free"`
		Offered       int64     `json:"offered"`       // free space offered to other sites' copies
		OfferedTotal  int64     `json:"offered_total"` // all of it (PeerShare of the storage)
		Generation    int64     `json:"generation"`
		PeerAdvertise string    `json:"peer_advertise"`
		WebHost       string    `json:"web_host,omitempty"` // guessed LAN host (NoteWebHost)
		Buckets       int64     `json:"buckets"`
		Objects       int64     `json:"objects"`
		ObjectBytes   int64     `json:"object_bytes"`
		HeldBlocks    int64     `json:"held_blocks"`
		HeldBytes     int64     `json:"held_bytes"`
		Location      *Location `json:"location,omitempty"`
		Restored      int64     `json:"restored"`
	} `json:"site"`
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
	d.Site.OfferedTotal, d.Site.Offered = s.offeredStorage()
	d.Site.Generation = s.SettingInt("generation")
	d.Site.PeerAdvertise = s.Setting("peer_advertise")
	d.Site.WebHost = s.WebHost()
	d.Site.Restored = s.SettingInt("restored")
	d.Site.Location = s.Location()
	s.DB.QueryRow(`SELECT COUNT(*) FROM buckets`).Scan(&d.Site.Buckets)
	s.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM objects`).Scan(&d.Site.Objects, &d.Site.ObjectBytes)
	s.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM held_blocks`).Scan(&d.Site.HeldBlocks, &d.Site.HeldBytes)
	d.Replication = s.ReplicationHealth()
	d.Capacity = s.IBSCapacity(d.Replication)
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
	var both, one int64
	s.DB.QueryRow(`SELECT COALESCE(SUM(CASE WHEN n = 0 THEN size END), 0), COALESCE(SUM(CASE WHEN n > 0 THEN size END), 0) FROM (
			SELECT b.size, (SELECT COUNT(*) FROM replicas r WHERE r.block_id = b.id AND r.state = 'ok') AS n
			FROM blocks b WHERE b.refs > 0)
		WHERE n < ?`, Copies).Scan(&both, &one)
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
