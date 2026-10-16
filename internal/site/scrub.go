package site

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ibs/internal/blockcodec"
	"ibs/internal/blockstore"
	"ibs/internal/config"
	"ibs/internal/keys"
)

// A scrub checks this site's data everywhere it is kept, like a ZFS scrub: its
// database and the encrypted copies of it on its nodes and on other sites,
// its own blocks, and the copies of them on other sites (scrubcopies.go).
// Storage nodes and other sites check the blocks on their own disks; only
// damaged blocks and challenge answers come back. Copies kept for other sites
// are checked when their owner scrubs, and here only if it hasn't for a long
// time.
//
//   - A damaged or missing block of ours is repaired from a copy on another
//     site. If no intact copy can be reached, the files using it are listed
//     and healing keeps trying.
//   - A damaged, missing or mismatching copy on another site is dropped and
//     copied again from this site.
//   - A damaged block kept for another site is dropped; that site notices
//     and sends a new copy.
//   - A damaged or missing copy of the database is sent again.
//
// Scrubs run on a schedule (monthly by default) and can be started, paused
// and stopped by hand. Reads are always checked, scrub or not (readBlock).
const (
	scrubBatch       = 32 // blocks per check on a node
	scrubKeep        = 6  // scrubs kept in the history
	defaultScrubDays = 30
)

// TopicScrub announces scrub progress.
const TopicScrub = "scrub"

type scrubState struct {
	mu        sync.Mutex
	wake      chan struct{}
	lastEvent time.Time
}

// Scrub is one scrub run.
type Scrub struct {
	ID       int64  `json:"id"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished,omitempty"`
	State    string `json:"state"` // running | paused | done | stopped
	Manual   bool   `json:"manual"`
	Phase    string `json:"phase"` // database | own | held
	Total    int64  `json:"total"` // blocks to check
	Checked  int64  `json:"checked"`
	Bytes    int64  `json:"bytes"`
	Repaired int64  `json:"repaired"`
	Failed   int64  `json:"failed"` // problems that couldn't be repaired
	// Copies of this site's blocks checked on other sites, and the bytes read there.
	Copies    int64 `json:"copies"`
	CopyBytes int64 `json:"copy_bytes"`
}

// ScrubError is something a scrub found.
type ScrubError struct {
	At       int64  `json:"at"`
	What     string `json:"what"` // block | remote | held | database | copy | node
	BlockID  string `json:"block_id,omitempty"`
	Place    string `json:"place,omitempty"` // where: a node or a site, by name
	Problem  string `json:"problem"`         // damaged | missing | unreachable
	Outcome  string `json:"outcome"`         // repaired | failed | replaced | skipped
	Detail   string `json:"detail,omitempty"`
	Resolved bool   `json:"resolved,omitempty"` // a failed block that has been healed since
}

// ScrubInfo is what the Scrub page and the dashboard show.
type ScrubInfo struct {
	Current      *Scrub       `json:"current"` // running or paused
	Last         *Scrub       `json:"last"`    // the latest finished
	History      []Scrub      `json:"history"`
	Errors       []ScrubError `json:"errors"` // of the current scrub, else the last
	IntervalDays int          `json:"interval_days"`
	NextDue      int64        `json:"next_due,omitempty"`
	Unresolved   int          `json:"unresolved"` // problems found that are still there
}

const scrubCols = `id, started, finished, state, manual, phase, total, checked, bytes, repaired, failed, copies, copy_bytes`

func scanScrub(row interface{ Scan(...any) error }) (Scrub, error) {
	var sc Scrub
	err := row.Scan(&sc.ID, &sc.Started, &sc.Finished, &sc.State, &sc.Manual, &sc.Phase, &sc.Total, &sc.Checked, &sc.Bytes, &sc.Repaired, &sc.Failed, &sc.Copies, &sc.CopyBytes)
	return sc, err
}

func (s *Site) currentScrub() *Scrub {
	sc, err := scanScrub(s.DB.QueryRow(`SELECT ` + scrubCols + ` FROM scrubs WHERE state IN ('running', 'paused') ORDER BY id DESC LIMIT 1`))
	if err != nil {
		return nil
	}
	return &sc
}

func (s *Site) lastScrub() *Scrub {
	sc, err := scanScrub(s.DB.QueryRow(`SELECT ` + scrubCols + ` FROM scrubs WHERE state IN ('done', 'stopped') ORDER BY id DESC LIMIT 1`))
	if err != nil {
		return nil
	}
	return &sc
}

// ScrubIntervalDays is how many days pass between scheduled scrubs (0 = none).
func (s *Site) ScrubIntervalDays() int {
	v := s.Setting("scrub_days")
	if v == "" {
		return defaultScrubDays
	}
	n, _ := strconv.Atoi(v)
	return max(n, 0)
}

// SetScrubInterval sets the days between scheduled scrubs (0 = none).
func (s *Site) SetScrubInterval(days int) error {
	if days < 0 || days > 365 {
		return errors.New("choose between 1 and 365 days, or 0 for no scheduled scrubs")
	}
	if err := s.SetSetting("scrub_days", strconv.Itoa(days)); err != nil {
		return err
	}
	s.wakeScrub()
	s.notify(TopicScrub)
	return nil
}

// nextScrubDue is when the next scheduled scrub starts (0 = none): an
// interval after the last one, or after this version first ran.
func (s *Site) nextScrubDue() int64 {
	days := s.ScrubIntervalDays()
	if days == 0 {
		return 0
	}
	since := s.SettingInt("scrub_since")
	if since == 0 {
		since = time.Now().Unix()
		s.SetSetting("scrub_since", strconv.FormatInt(since, 10))
	}
	if last := s.lastScrub(); last != nil {
		since = max(since, last.Started)
	}
	return since + int64(days)*86400
}

// StartScrub starts a scrub now.
func (s *Site) StartScrub(manual bool) error {
	s.scrub.mu.Lock()
	defer s.scrub.mu.Unlock()
	if s.currentScrub() != nil {
		return errors.New("a scrub is already running")
	}
	var blocks, copies, held int64
	s.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE refs > 0`).Scan(&blocks)
	s.DB.QueryRow(`SELECT COUNT(*) FROM replicas r JOIN blocks b ON b.id = r.block_id WHERE b.refs > 0 AND r.state = 'ok'`).Scan(&copies)
	s.DB.QueryRow(`SELECT COUNT(*) FROM held_blocks WHERE verified < ?`, time.Now().Add(-heldSelfCheckAfter).Unix()).Scan(&held)
	if _, err := s.DB.Exec(`INSERT INTO scrubs(started, state, manual, total) VALUES(?, 'running', ?, ?)`,
		time.Now().Unix(), manual, blocks+copies+held); err != nil {
		return err
	}
	// Keep the latest few.
	s.DB.Exec(`DELETE FROM scrubs WHERE id NOT IN (SELECT id FROM scrubs ORDER BY id DESC LIMIT ?)`, scrubKeep)
	s.DB.Exec(`DELETE FROM scrub_errors WHERE scrub_id NOT IN (SELECT id FROM scrubs)`)
	s.DB.Exec(`DELETE FROM scrub_copies WHERE scrub_id NOT IN (SELECT id FROM scrubs)`)
	s.log.Info("scrub started", "manual", manual, "blocks", blocks, "copies_elsewhere", copies, "kept_for_others", held)
	s.wakeScrub()
	s.notify(TopicScrub)
	return nil
}

// PauseScrub, ResumeScrub and StopScrub control the running scrub.
func (s *Site) PauseScrub() error  { return s.setScrubState("running", "paused") }
func (s *Site) ResumeScrub() error { return s.setScrubState("paused", "running") }
func (s *Site) StopScrub() error {
	s.scrub.mu.Lock()
	defer s.scrub.mu.Unlock()
	res, err := s.DB.Exec(`UPDATE scrubs SET state = 'stopped', finished = ? WHERE state IN ('running', 'paused')`, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no scrub is running")
	}
	s.log.Info("scrub stopped")
	s.wakeScrub()
	s.notify(TopicScrub)
	return nil
}

func (s *Site) setScrubState(from, to string) error {
	s.scrub.mu.Lock()
	defer s.scrub.mu.Unlock()
	res, err := s.DB.Exec(`UPDATE scrubs SET state = ? WHERE state = ?`, to, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no scrub is %s", from)
	}
	s.wakeScrub()
	s.notify(TopicScrub)
	return nil
}

func (s *Site) wakeScrub() {
	select {
	case s.scrub.wake <- struct{}{}:
	default:
	}
}

// scrubRate is how many bytes per second a scrub may read, so it doesn't
// slow down backups running at the same time (IBS_SCRUB_RATE).
func scrubRate() int64 {
	if v := os.Getenv("IBS_SCRUB_RATE"); v != "" {
		if n, err := config.ParseSize(v); err == nil && n > 0 {
			return n
		}
	}
	return 100 << 20
}

// scrubLoop runs scrubs: the one in progress (also after a restart), and
// scheduled ones when they are due.
func (s *Site) scrubLoop(ctx context.Context) {
	for {
		if sc := s.currentScrub(); sc != nil {
			if sc.State == "running" {
				s.runScrub(ctx, sc.ID)
			}
		} else if due := s.nextScrubDue(); due != 0 && time.Now().Unix() >= due {
			if err := s.StartScrub(false); err != nil {
				s.log.Warn("start scheduled scrub", "err", err)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.scrub.wake:
		case <-time.After(time.Minute):
		}
	}
}

func (s *Site) scrubEvent(force bool) {
	s.scrub.mu.Lock()
	due := force || time.Since(s.scrub.lastEvent) > 2*time.Second
	if due {
		s.scrub.lastEvent = time.Now()
	}
	s.scrub.mu.Unlock()
	if due {
		s.notify(TopicScrub)
	}
}

// runScrub works through a scrub batch by batch until it is done, paused,
// stopped, or the site shuts down. Progress is saved after every batch.
func (s *Site) runScrub(ctx context.Context, id int64) {
	r := &scrubRun{s: s, id: id, unreachable: map[string]bool{}, skipped: map[string]bool{}}
	rate := scrubRate()
	for ctx.Err() == nil {
		var state, phase, cursor string
		if err := s.DB.QueryRow(`SELECT state, phase, cursor FROM scrubs WHERE id = ?`, id).Scan(&state, &phase, &cursor); err != nil || state != "running" {
			return
		}
		var n, bytes int64
		var next string
		switch phase {
		case "database":
			r.database(ctx)
			next = "own"
		case "own":
			n, bytes, cursor = r.ownBatch(ctx, cursor)
			if n == 0 {
				next = "held"
			}
		case "held":
			n, bytes, cursor = r.heldBatch(ctx, cursor)
			if n == 0 {
				next = "done"
			}
		default:
			next = "done"
		}
		if ctx.Err() != nil {
			return // the batch was cut short; it runs again after the restart
		}
		switch next {
		case "":
			s.DB.Exec(`UPDATE scrubs SET cursor = ?, checked = checked + ?, bytes = bytes + ? WHERE id = ? AND state = 'running'`, cursor, n, bytes, id)
		case "done":
			s.DB.Exec(`UPDATE scrubs SET state = 'done', finished = ?, checked = checked + ?, bytes = bytes + ? WHERE id = ? AND state = 'running'`,
				time.Now().Unix(), n, bytes, id)
			sc := s.lastScrub()
			if sc != nil {
				s.log.Info("scrub done", "checked", sc.Checked, "repaired", sc.Repaired, "failed", sc.Failed)
			}
			s.scrubEvent(true)
			return
		default:
			s.DB.Exec(`UPDATE scrubs SET phase = ?, cursor = '', checked = checked + ?, bytes = bytes + ? WHERE id = ? AND state = 'running'`, next, n, bytes, id)
		}
		s.scrubEvent(false)
		// Keep to the rate, shared with checks other sites ask for.
		s.scrubLimit.wait(ctx, bytes, rate)
	}
}

type scrubRun struct {
	s           *Site
	id          int64
	unreachable map[string]bool // nodes already reported as unreachable in this run
	skipped     map[string]bool // sites whose copies weren't checked, already reported
}

func (r *scrubRun) report(e ScrubError) {
	r.s.DB.Exec(`INSERT INTO scrub_errors(scrub_id, at, what, block_id, place, problem, outcome, detail) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		r.id, time.Now().Unix(), e.What, e.BlockID, e.Place, e.Problem, e.Outcome, e.Detail)
	switch e.Outcome {
	case "repaired", "replaced":
		r.s.DB.Exec(`UPDATE scrubs SET repaired = repaired + 1 WHERE id = ?`, r.id)
	case "failed":
		r.s.DB.Exec(`UPDATE scrubs SET failed = failed + 1 WHERE id = ?`, r.id)
	}
	if e.Outcome == "failed" {
		r.s.log.Error("scrub: could not repair", "what", e.What, "block", e.BlockID, "place", e.Place, "problem", e.Problem, "detail", e.Detail)
	} else {
		r.s.log.Warn("scrub: found a problem", "what", e.What, "block", e.BlockID, "place", e.Place, "problem", e.Problem, "outcome", e.Outcome)
	}
	r.s.scrubEvent(true)
}

func (r *scrubRun) nodeUnreachable(node string, n int, err error) {
	if r.unreachable[node] {
		return
	}
	r.unreachable[node] = true
	r.report(ScrubError{What: "node", Place: node, Problem: "unreachable", Outcome: "skipped",
		Detail: fmt.Sprintf("its blocks were not checked: %v", err)})
}

// verifyBlocks checks blocks on one node, on the node itself, and answers the
// challenge for intact ones when a nonce is given.
func (s *Site) verifyBlocks(ctx context.Context, st blockstore.Store, ns string, ids []string, nonce []byte) (blockstore.VerifyResult, error) {
	if v, ok := st.(blockstore.Verifier); ok {
		return v.Verify(ctx, ns, ids, nonce)
	}
	// A store that can't check itself (only in tests): read the blocks here.
	var res blockstore.VerifyResult
	for _, id := range ids {
		data, err := st.Get(ctx, ns, id)
		switch {
		case errors.Is(err, blockstore.ErrNotFound):
			res.Damaged = append(res.Damaged, blockstore.Damaged{ID: id, Missing: true})
		case err != nil:
			return res, err
		default:
			res.Bytes += int64(len(data))
			if !blockcodec.Verify(id, data) {
				res.Damaged = append(res.Damaged, blockstore.Damaged{ID: id})
			} else if nonce != nil {
				if res.Answers == nil {
					res.Answers = map[string]string{}
				}
				res.Answers[id] = blockstore.ChallengeAnswer(nonce, data)
			}
		}
	}
	return res, nil
}

// database checks the database and its encrypted copies on the site's
// storage nodes and on other sites; damaged or missing copies are sent again.
func (r *scrubRun) database(ctx context.Context) {
	s := r.s
	var result string
	if err := s.DB.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		result = err.Error()
	}
	if result != "ok" {
		r.report(ScrubError{What: "database", Problem: "damaged", Outcome: "failed",
			Detail: "the site's database doesn't pass SQLite's integrity check: " + result})
	}
	open := func(blob []byte) bool {
		_, err := keys.Open(s.ID.Master, blob, backupAAD(s.ID.SiteID))
		return err == nil
	}
	for _, id := range s.onlineRemoteNodes() {
		rm, ok := s.nodeStore(id).(*blockstore.Remote)
		if !ok {
			continue
		}
		_, blob, err := rm.GetMetadata(ctx)
		problem := ""
		switch {
		case err != nil && strings.Contains(err.Error(), "404"):
			problem = "missing"
		case err != nil:
			continue // unreachable; the blocks phase reports it
		case !open(blob):
			problem = "damaged"
		}
		if problem != "" {
			s.peerMu.Lock()
			delete(s.nodeMetaGen, id)
			s.peerMu.Unlock()
			r.report(ScrubError{What: "copy", Place: s.nodeName(id), Problem: problem, Outcome: "repaired",
				Detail: "the copy of the site's database on this node is sent again"})
		}
	}
	for _, p := range s.onlinePeers() {
		var blob []byte
		_, err := s.peerCall(ctx, p.siteID, http.MethodGet, "/peer/v1/backup", nil, &blob)
		problem := ""
		switch {
		case isStatus(err, http.StatusNotFound):
			problem = "missing"
		case err != nil:
			continue
		case !open(blob):
			problem = "damaged"
		}
		if problem != "" {
			s.peerMu.Lock()
			delete(s.pushedGen, p.siteID)
			s.peerMu.Unlock()
			r.report(ScrubError{What: "copy", Place: s.peerName(p.siteID), Problem: problem, Outcome: "repaired",
				Detail: "the copy of the site's database on this site is sent again"})
		}
	}
}

// ownBatch checks the next batch of this site's blocks after cursor.
func (r *scrubRun) ownBatch(ctx context.Context, cursor string) (n, bytes int64, next string) {
	s := r.s
	rows, err := s.DB.QueryContext(ctx, `SELECT id, node_id FROM blocks WHERE refs > 0 AND id > ? ORDER BY id LIMIT ?`, cursor, 8*scrubBatch)
	if err != nil {
		return 0, 0, cursor
	}
	byNode := map[string][]string{}
	var order []string
	for rows.Next() {
		var id, node string
		rows.Scan(&id, &node)
		if _, ok := byNode[node]; !ok {
			order = append(order, node)
		}
		byNode[node] = append(byNode[node], id)
		next = id
		n++
	}
	rows.Close()
	if n == 0 {
		return 0, 0, cursor
	}
	// The copies on other sites are checked at the same time, with the same
	// challenge; the answers are compared once both are in.
	nonce := newNonce()
	var all []string
	for _, node := range order {
		all = append(all, byNode[node]...)
	}
	remote := r.checkCopies(ctx, all, nonce)
	local := map[string]string{}
	defer func() { r.compareCopies(ctx, remote, local) }()

	now := time.Now().Unix()
	for _, node := range order {
		ids := byNode[node]
		if node == "" {
			// Missing here already; healing is on it, but try now.
			for _, id := range ids {
				r.repairOwn(ctx, id, "", true)
			}
			continue
		}
		st := s.nodeStore(node)
		if st == nil {
			continue
		}
		for i := 0; i < len(ids); i += scrubBatch {
			chunk := ids[i:min(i+scrubBatch, len(ids))]
			res, err := s.verifyBlocks(ctx, st, blockstore.OwnNS, chunk, nonce)
			if err != nil {
				if ctx.Err() == nil {
					r.nodeUnreachable(s.nodeName(node), len(chunk), err)
				}
				continue
			}
			bytes += res.Bytes
			for id, a := range res.Answers {
				local[id] = a
			}
			bad := map[string]bool{}
			for _, d := range res.Damaged {
				bad[d.ID] = true
				r.repairOwn(ctx, d.ID, node, d.Missing)
			}
			for _, id := range chunk {
				if !bad[id] {
					s.DB.Exec(`UPDATE blocks SET verified = ? WHERE id = ?`, now, id)
				}
			}
		}
	}
	return n, bytes, next
}

// repairOwn replaces a damaged or missing block of ours with a copy from
// another site.
func (r *scrubRun) repairOwn(ctx context.Context, id, node string, missing bool) {
	s := r.s
	problem := "damaged"
	if missing {
		problem = "missing"
	}
	place := "this site"
	if node != "" {
		place = s.nodeName(node)
		// Forget the bad copy so it is healed, now or later.
		s.DB.Exec(`UPDATE blocks SET node_id = '' WHERE id = ? AND node_id = ?`, id, node)
		if st := s.nodeStore(node); st != nil && !missing {
			st.Delete(ctx, blockstore.OwnNS, id)
		}
	}
	ct, from, err := s.fetchFromReplicas(ctx, id)
	if err == nil {
		err = s.healBlock(ctx, id, ct, "")
	}
	if err != nil {
		r.report(ScrubError{What: "block", BlockID: id, Place: place, Problem: problem, Outcome: "failed",
			Detail: "no intact copy could be reached; IBS keeps trying. Files using it: " + s.filesUsing(id)})
		return
	}
	r.report(ScrubError{What: "block", BlockID: id, Place: place, Problem: problem, Outcome: "repaired",
		Detail: "from the copy on " + s.peerName(from)})
}

// filesUsing lists (some of) the files that use a block.
func (s *Site) filesUsing(id string) string {
	rows, err := s.DB.Query(`SELECT bucket, key FROM objects WHERE blocks LIKE ? LIMIT 10`, `%"`+id+`"%`)
	if err != nil {
		return "unknown"
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var b, k string
		rows.Scan(&b, &k)
		files = append(files, b+"/"+k)
	}
	if len(files) == 0 {
		return "none (an unfinished upload)"
	}
	return strings.Join(files, ", ")
}

// heldBatch checks the next batch of blocks kept for other sites after cursor
// (owner/id). Damaged ones are dropped; their owner sends new copies.
func (r *scrubRun) heldBatch(ctx context.Context, cursor string) (n, bytes int64, next string) {
	s := r.s
	owner, id, _ := strings.Cut(cursor, "/")
	// Only copies their owner hasn't checked for a while: owners check theirs.
	rows, err := s.DB.QueryContext(ctx, `SELECT owner, id, node_id FROM held_blocks WHERE (owner, id) > (?, ?) AND verified < ?
		ORDER BY owner, id LIMIT ?`, owner, id, time.Now().Add(-heldSelfCheckAfter).Unix(), 8*scrubBatch)
	if err != nil {
		return 0, 0, cursor
	}
	type key struct{ node, owner string }
	groups := map[key][]string{}
	var order []key
	for rows.Next() {
		var o, b, node string
		rows.Scan(&o, &b, &node)
		k := key{node, o}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], b)
		next = o + "/" + b
		n++
	}
	rows.Close()
	if n == 0 {
		return 0, 0, cursor
	}
	now := time.Now().Unix()
	for _, k := range order {
		ids := groups[k]
		st := s.nodeStore(k.node)
		if st == nil {
			// The node is gone; so are the blocks.
			for _, b := range ids {
				s.DB.Exec(`DELETE FROM held_blocks WHERE owner = ? AND id = ?`, k.owner, b)
			}
			continue
		}
		for i := 0; i < len(ids); i += scrubBatch {
			chunk := ids[i:min(i+scrubBatch, len(ids))]
			res, err := s.verifyBlocks(ctx, st, k.owner, chunk, nil)
			if err != nil {
				if ctx.Err() == nil {
					r.nodeUnreachable(s.nodeName(k.node), len(chunk), err)
				}
				continue
			}
			bytes += res.Bytes
			bad := map[string]bool{}
			for _, d := range res.Damaged {
				bad[d.ID] = true
				st.Delete(ctx, k.owner, d.ID)
				s.DB.Exec(`DELETE FROM held_blocks WHERE owner = ? AND id = ?`, k.owner, d.ID)
				problem := "damaged"
				if d.Missing {
					problem = "missing"
				}
				r.report(ScrubError{What: "held", BlockID: d.ID, Place: s.peerName(k.owner), Problem: problem, Outcome: "replaced",
					Detail: "a copy kept for " + s.peerName(k.owner) + " on " + s.nodeName(k.node) + "; that site sends a new one"})
			}
			for _, b := range chunk {
				if !bad[b] {
					s.DB.Exec(`UPDATE held_blocks SET verified = ? WHERE owner = ? AND id = ?`, now, k.owner, b)
				}
			}
		}
	}
	return n, bytes, next
}

func (s *Site) nodeName(id string) string {
	var name string
	if s.DB.QueryRow(`SELECT name FROM nodes WHERE id = ?`, id).Scan(&name) != nil || name == "" {
		return id
	}
	return name
}

func (s *Site) peerName(id string) string {
	var name string
	if s.DB.QueryRow(`SELECT name FROM peers WHERE site_id = ?`, id).Scan(&name) != nil &&
		s.DB.QueryRow(`SELECT name FROM network WHERE site_id = ?`, id).Scan(&name) != nil {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}
	return name
}

// ScrubStatus is what the Scrub page and the dashboard show.
func (s *Site) ScrubStatus() ScrubInfo {
	info := ScrubInfo{Current: s.currentScrub(), Last: s.lastScrub(), History: []Scrub{}, Errors: []ScrubError{},
		IntervalDays: s.ScrubIntervalDays(), NextDue: s.nextScrubDue()}
	if rows, err := s.DB.Query(`SELECT `+scrubCols+` FROM scrubs ORDER BY id DESC LIMIT ?`, scrubKeep); err == nil {
		for rows.Next() {
			if sc, err := scanScrub(rows); err == nil {
				info.History = append(info.History, sc)
			}
		}
		rows.Close()
	}
	shown := info.Current
	if shown == nil {
		shown = info.Last
	}
	if shown == nil {
		return info
	}
	info.Errors = s.scrubErrors(shown.ID)
	// Problems that are still there: from the shown scrub, and from the last
	// finished one while another runs, until a scrub finds none.
	ids := []int64{shown.ID}
	if info.Current != nil && info.Last != nil {
		ids = append(ids, info.Last.ID)
	}
	for _, id := range ids {
		errs := info.Errors
		if id != shown.ID {
			errs = s.scrubErrors(id)
		}
		for _, e := range errs {
			if e.Outcome == "failed" && !e.Resolved {
				info.Unresolved++
			}
		}
	}
	return info
}

func (s *Site) scrubErrors(id int64) []ScrubError {
	out := []ScrubError{}
	rows, err := s.DB.Query(`SELECT at, what, block_id, place, problem, outcome, detail FROM scrub_errors WHERE scrub_id = ? ORDER BY at, rowid LIMIT 1000`, id)
	if err != nil {
		return out
	}
	for rows.Next() {
		var e ScrubError
		rows.Scan(&e.At, &e.What, &e.BlockID, &e.Place, &e.Problem, &e.Outcome, &e.Detail)
		out = append(out, e)
	}
	rows.Close()
	for i, e := range out {
		if e.What == "block" && e.Outcome == "failed" {
			var node string
			var refs int
			err := s.DB.QueryRow(`SELECT node_id, refs FROM blocks WHERE id = ?`, e.BlockID).Scan(&node, &refs)
			out[i].Resolved = err != nil || refs <= 0 || node != ""
		}
	}
	return out
}

// CopyCheck is how the copies of this site's blocks on another site did in
// this site's last finished scrub (Scrubbed 0: not checked yet).
type CopyCheck struct {
	SiteID   string `json:"site_id"`
	Name     string `json:"name"`
	Copies   int64  `json:"copies"` // copies of our blocks there now
	Scrubbed int64  `json:"scrubbed"`
	Checked  int64  `json:"checked"`
	Bad      int64  `json:"bad"` // missing, damaged or failing the challenge; copied again
}

// CopyChecks lists the sites holding copies of this site's blocks, and those
// checked in the last scrub (their copies may have been dropped since, to be
// copied again), with what that scrub found there.
func (s *Site) CopyChecks() []CopyCheck {
	last := s.lastScrub()
	var lastID, finished int64
	if last != nil && last.State == "done" {
		lastID, finished = last.ID, last.Finished
	}
	rows, err := s.DB.Query(`SELECT site_id, SUM(n), SUM(checked), SUM(bad) FROM (
			SELECT site_id, COUNT(*) AS n, 0 AS checked, 0 AS bad FROM replicas WHERE state = 'ok' GROUP BY site_id
			UNION ALL
			SELECT site_id, 0, checked, bad FROM scrub_copies WHERE scrub_id = ?)
		GROUP BY site_id ORDER BY site_id`, lastID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CopyCheck
	for rows.Next() {
		var c CopyCheck
		rows.Scan(&c.SiteID, &c.Copies, &c.Checked, &c.Bad)
		c.Name = s.peerName(c.SiteID)
		if c.Checked > 0 {
			c.Scrubbed = finished
		}
		out = append(out, c)
	}
	return out
}
