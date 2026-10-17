// Package db opens the site's SQLite database and applies the schema.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);

-- Storage nodes at this site, including the manager itself (is_local = 1).
-- disk_* is what the node reports; quota caps what IBS may store on it
-- (0 = whole disk); capacity/free are the effective values within the quota.
CREATE TABLE IF NOT EXISTS nodes (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	address       TEXT NOT NULL,
	secret        TEXT NOT NULL, -- unused since nodes authenticate with keys
	pubkey        BLOB NOT NULL DEFAULT x'',
	quota         INTEGER NOT NULL DEFAULT 0,
	disk_capacity INTEGER NOT NULL DEFAULT 0,
	disk_free     INTEGER NOT NULL DEFAULT 0,
	capacity      INTEGER NOT NULL DEFAULT 0,
	free          INTEGER NOT NULL DEFAULT 0,
	last_seen INTEGER NOT NULL DEFAULT 0,
	is_local  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS buckets (
	name        TEXT PRIMARY KEY,
	created     INTEGER NOT NULL,
	replication INTEGER NOT NULL,
	wrapped_key BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS objects (
	bucket       TEXT NOT NULL,
	key          TEXT NOT NULL,
	size         INTEGER NOT NULL,
	etag         TEXT NOT NULL,
	content_type TEXT NOT NULL,
	modified     INTEGER NOT NULL,
	user_meta    TEXT NOT NULL DEFAULT '{}',
	blocks       TEXT NOT NULL, -- JSON [{"id":..,"size":plain size}]
	PRIMARY KEY (bucket, key)
);

-- Blocks owned by this site. Each belongs to one file or multipart part (refs
-- 1; 0 when unused: deleted, unless an upload in progress owns it). node_id is
-- the local node holding it ('' when missing locally).
CREATE TABLE IF NOT EXISTS blocks (
	id      TEXT PRIMARY KEY,
	bucket  TEXT NOT NULL,
	size    INTEGER NOT NULL,
	refs    INTEGER NOT NULL DEFAULT 0,
	node_id  TEXT NOT NULL DEFAULT '',
	touched  INTEGER NOT NULL,
	verified INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS blocks_refs ON blocks(refs);
CREATE INDEX IF NOT EXISTS blocks_node ON blocks(node_id);

-- Copies of our blocks held by other sites. state: ok | delete
CREATE TABLE IF NOT EXISTS replicas (
	block_id TEXT NOT NULL,
	site_id  TEXT NOT NULL,
	state    TEXT NOT NULL DEFAULT 'ok',
	verified INTEGER NOT NULL,
	PRIMARY KEY (block_id, site_id)
);
CREATE INDEX IF NOT EXISTS replicas_site ON replicas(site_id);

-- Directly paired sites.
CREATE TABLE IF NOT EXISTS peers (
	site_id      TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	pubkey       BLOB NOT NULL,
	address      TEXT NOT NULL,
	added        INTEGER NOT NULL,
	last_seen    INTEGER NOT NULL DEFAULT 0,
	online       INTEGER NOT NULL DEFAULT 0,
	up_since     INTEGER NOT NULL DEFAULT 0,
	capacity     INTEGER NOT NULL DEFAULT 0,
	free         INTEGER NOT NULL DEFAULT 0,
	last_error   TEXT NOT NULL DEFAULT '',
	reconciled   INTEGER NOT NULL DEFAULT 0
);

-- Every site known in the IBS, learned from peers. Only public facts.
-- Sites that left the IBS, with the time they left (by their own clock). They
-- are not connected to again unless they rejoin later than that.
CREATE TABLE IF NOT EXISTS left_sites (
	site_id TEXT PRIMARY KEY,
	at      INTEGER NOT NULL
);

-- On a site that left the IBS: its former members, told so when they call.
CREATE TABLE IF NOT EXISTS former_members (
	site_id TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS network (
	site_id   TEXT PRIMARY KEY,
	name      TEXT NOT NULL,
	up_since  INTEGER NOT NULL,
	capacity  INTEGER NOT NULL,
	free      INTEGER NOT NULL,
	online    INTEGER NOT NULL,
	last_seen INTEGER NOT NULL
);

-- Blocks this site stores on behalf of other sites.
CREATE TABLE IF NOT EXISTS held_blocks (
	owner    TEXT NOT NULL,
	id       TEXT NOT NULL,
	size     INTEGER NOT NULL,
	node_id  TEXT NOT NULL,
	stored   INTEGER NOT NULL,
	verified INTEGER NOT NULL,
	PRIMARY KEY (owner, id)
);
CREATE INDEX IF NOT EXISTS held_verified ON held_blocks(verified);

-- Encrypted metadata backups other sites have pushed to us.
CREATE TABLE IF NOT EXISTS peer_backups (
	owner      TEXT PRIMARY KEY,
	generation INTEGER NOT NULL,
	size       INTEGER NOT NULL,
	updated    INTEGER NOT NULL
);

-- Relays this site is registered with.
CREATE TABLE IF NOT EXISTS relays (
	id      TEXT PRIMARY KEY,
	address TEXT NOT NULL,
	pubkey  BLOB NOT NULL,
	secret  TEXT NOT NULL DEFAULT '',
	added   INTEGER NOT NULL
);

-- Relays removed by hand here or on another site, and when: a relay another
-- site still lists is only added again if it was added there after that.
CREATE TABLE IF NOT EXISTS removed_relays (
	id TEXT PRIMARY KEY,
	at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS invites (
	secret  TEXT PRIMARY KEY,
	created INTEGER NOT NULL,
	expires INTEGER NOT NULL,
	used_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS access_keys (
	id         TEXT PRIMARY KEY,
	secret_enc BLOB NOT NULL,
	name       TEXT NOT NULL,
	created    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS uploads (
	id           TEXT PRIMARY KEY,
	bucket       TEXT NOT NULL,
	key          TEXT NOT NULL,
	created      INTEGER NOT NULL,
	content_type TEXT NOT NULL,
	user_meta    TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS parts (
	upload_id TEXT NOT NULL,
	num       INTEGER NOT NULL,
	etag      TEXT NOT NULL,
	size      INTEGER NOT NULL,
	blocks    TEXT NOT NULL,
	modified  INTEGER NOT NULL,
	PRIMARY KEY (upload_id, num)
);

-- Scrubs: full checks of everything this site stores. The latest few are kept.
-- state: running | paused | done | stopped. phase: database | own | held,
-- cursor the last block id checked in it, so a scrub resumes after a restart.
CREATE TABLE IF NOT EXISTS scrubs (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	started  INTEGER NOT NULL,
	finished INTEGER NOT NULL DEFAULT 0,
	state    TEXT NOT NULL,
	manual   INTEGER NOT NULL DEFAULT 0,
	phase    TEXT NOT NULL DEFAULT 'database',
	cursor   TEXT NOT NULL DEFAULT '',
	total    INTEGER NOT NULL DEFAULT 0,
	checked  INTEGER NOT NULL DEFAULT 0,
	bytes    INTEGER NOT NULL DEFAULT 0,
	repaired INTEGER NOT NULL DEFAULT 0,
	failed   INTEGER NOT NULL DEFAULT 0
);

-- What a scrub found. what: block (ours) | held (kept for another site) |
-- database | copy (an encrypted copy of the database on a node or site).
-- outcome: repaired | failed | replaced (a copy kept for another site was
-- dropped; the owner sends a new one).
CREATE TABLE IF NOT EXISTS scrub_errors (
	scrub_id INTEGER NOT NULL,
	at       INTEGER NOT NULL,
	what     TEXT NOT NULL,
	block_id TEXT NOT NULL DEFAULT '',
	place    TEXT NOT NULL DEFAULT '',
	problem  TEXT NOT NULL,
	outcome  TEXT NOT NULL,
	detail   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS scrub_errors_scrub ON scrub_errors(scrub_id);

-- Per site: how the copies of this site's blocks there did in this site's
-- scrub (checked; bad: missing, damaged or failing the challenge).
CREATE TABLE IF NOT EXISTS scrub_copies (
	scrub_id INTEGER NOT NULL,
	site_id  TEXT NOT NULL,
	checked  INTEGER NOT NULL DEFAULT 0,
	bad      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (scrub_id, site_id)
);

-- Copies of this site's blocks a scrub couldn't check because their site was
-- unreachable; checked at the end of the scrub if the site is back.
CREATE TABLE IF NOT EXISTS scrub_skipped (
	scrub_id INTEGER NOT NULL,
	site_id  TEXT NOT NULL,
	block_id TEXT NOT NULL,
	PRIMARY KEY (scrub_id, site_id, block_id)
);

-- Short invite codes this site gave out: the number claimed on a relay, the
-- words both sites derive their key from, and the full invite sent once the
-- joining site proves it knows the words. used: the one attempt was made.
CREATE TABLE IF NOT EXISTS invite_codes (
	number   TEXT PRIMARY KEY,
	relay_id TEXT NOT NULL,
	words    TEXT NOT NULL,
	invite   TEXT NOT NULL,
	expires  INTEGER NOT NULL,
	used     INTEGER NOT NULL DEFAULT 0
);
`

// Level is how many database changes this version knows. A database changed
// by a newer version (a higher level) can't be used by an older one.
var Level = len(migrations)

var migrations = []string{
	`ALTER TABLE nodes ADD COLUMN quota INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE nodes ADD COLUMN disk_capacity INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE nodes ADD COLUMN disk_free INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE nodes ADD COLUMN pubkey BLOB NOT NULL DEFAULT x''`,
	`ALTER TABLE peers ADD COLUMN routes TEXT NOT NULL DEFAULT '{}'`,
	// Storage limits: env_limit is IBS_STORAGE_LIMIT as the node reports it (0 = not set),
	// ui_limit the one set in the web interface (NULL = not set). quota is the one in effect.
	`ALTER TABLE nodes ADD COLUMN env_limit INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE nodes ADD COLUMN ui_limit INTEGER`,
	// IBS membership: what a site needs to connect to another member of the IBS.
	`ALTER TABLE network ADD COLUMN pubkey BLOB NOT NULL DEFAULT x''`,
	`ALTER TABLE network ADD COLUMN address TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE network ADD COLUMN relay_tokens TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE peers ADD COLUMN relay_tokens TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE peers ADD COLUMN joined INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE network ADD COLUMN joined INTEGER NOT NULL DEFAULT 0`,
	// Where sites are shown on the map (JSON, '' = none).
	`ALTER TABLE peers ADD COLUMN location TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE network ADD COLUMN location TEXT NOT NULL DEFAULT ''`,
	// Every file is kept on two other sites (site.Copies); buckets once had their own count.
	`UPDATE buckets SET replication = 2 WHERE replication != 2`,
	// The version each node and site runs, as it reports it.
	`ALTER TABLE nodes ADD COLUMN version TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE peers ADD COLUMN version TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE network ADD COLUMN version TEXT NOT NULL DEFAULT ''`,
	// Updates: the versions each connected site holds, and the oldest version
	// each site works with.
	`ALTER TABLE peers ADD COLUMN releases TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE peers ADD COLUMN min_compatible TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE network ADD COLUMN min_compatible TEXT NOT NULL DEFAULT ''`,
	// Relay renames: when a relay's address was set by hand (0 = never), here
	// and as each connected site reports it, so the newest address spreads.
	`ALTER TABLE relays ADD COLUMN address_set INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE peers ADD COLUMN relay_addresses TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE peers ADD COLUMN relay_removals TEXT NOT NULL DEFAULT '[]'`,
	// Scrubs check the copies of a site's blocks on other sites too.
	`ALTER TABLE scrubs ADD COLUMN copies INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE scrubs ADD COLUMN copy_bytes INTEGER NOT NULL DEFAULT 0`,
	// The upload in progress a block belongs to until it's committed ('' = none):
	// the garbage collector leaves those alone, however long the upload takes.
	`ALTER TABLE blocks ADD COLUMN upload TEXT NOT NULL DEFAULT ''`,
	// Copies a finished scrub couldn't check: their site was down to the end.
	`ALTER TABLE scrubs ADD COLUMN unchecked INTEGER NOT NULL DEFAULT 0`,
}

func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate",
		url.PathEscape(path))
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(8)
	if _, err := d.ExecContext(context.Background(), schema); err != nil {
		d.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Columns added after the first release; existing databases get them here.
	for _, m := range migrations {
		if _, err := d.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			d.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return d, nil
}

func GetSetting(d interface {
	QueryRow(string, ...any) *sql.Row
}, k string) string {
	var v string
	d.QueryRow(`SELECT v FROM settings WHERE k = ?`, k).Scan(&v)
	return v
}

func SetSetting(d interface {
	Exec(string, ...any) (sql.Result, error)
}, k, v string) error {
	_, err := d.Exec(`INSERT INTO settings(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}
