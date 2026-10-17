package site

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"ibs/internal/blockstore"
)

// A site's scrub also checks the copies of its blocks on other sites: for
// each batch of its blocks it sends the sites holding copies the block ids
// and a fresh random nonce. Each answers, per block, the SHA-256 of the nonce
// and the bytes it holds (blockstore.ChallengeAnswer), and its storage node
// works out the same for this site's own intact copy. Matching answers prove
// the other site holds the exact bytes right now; this site doesn't take its
// word for it. Missing, damaged and mismatching copies are dropped and copied
// again from this site.
//
// The site holding the copies reads them at its own scrub rate, whoever asks,
// and only about copyCheckWork worth per request: it answers what it got
// through and hands back the rest, or says when to come back if it's busy.
// So every request ends within seconds, and a site that doesn't answer is
// found out within peerTimeout.
// It still checks copies itself that their owner hasn't checked for a while
// (heldSelfCheckAfter), e.g. of a site that has been offline for long.

const heldSelfCheckAfter = 60 * 24 * time.Hour

// copyCheckWork is how much reading one copy check request does, at the
// scrub rate. A variable for tests.
var copyCheckWork = 10 * time.Second

// byteLimiter keeps reads to a rate across everything that reads for scrubs.
type byteLimiter struct {
	mu   sync.Mutex
	next time.Time
}

// backlog is how long reads already allowed take to fit in the rate.
func (l *byteLimiter) backlog() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(time.Until(l.next), 0)
}

// reserve counts n bytes against the rate and returns when they fit.
func (l *byteLimiter) reserve(n, rate int64) time.Time {
	d := time.Duration(float64(n) / float64(rate) * float64(time.Second))
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(d)
	return l.next
}

// wait returns when n more bytes fit in the rate.
func (l *byteLimiter) wait(ctx context.Context, n, rate int64) {
	until := l.reserve(n, rate)
	select {
	case <-ctx.Done():
	case <-time.After(time.Until(until)):
	}
}

type copyCheckRequest struct {
	Nonce []byte   `json:"nonce"`
	IDs   []string `json:"ids"`
}

type copyCheckReply struct {
	Answers   map[string]string `json:"answers"`   // intact copies: the challenge answer ("" if a node can't answer)
	Damaged   []string          `json:"damaged"`   // didn't match their id; dropped
	Missing   []string          `json:"missing"`   // not here
	Unchecked []string          `json:"unchecked"` // on a node that couldn't be reached
	Later     []string          `json:"later"`     // not got to in this request: ask again
	Wait      float64           `json:"wait"`      // seconds to wait before asking again (busy)
	Bytes     int64             `json:"bytes"`
}

// handleCopyCheck is the holder's side: check the caller's copies and answer
// the challenge.
func (s *Site) handleCopyCheck(w http.ResponseWriter, r *http.Request) {
	owner := callerOf(r).siteID
	var req copyCheckRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil || len(req.Nonce) < 16 || len(req.IDs) > 4096 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	writeJSON(w, s.checkHeld(r.Context(), owner, req.IDs, req.Nonce))
}

func (s *Site) checkHeld(ctx context.Context, owner string, ids []string, nonce []byte) copyCheckReply {
	reply := copyCheckReply{Answers: map[string]string{}, Damaged: []string{}, Missing: []string{}, Unchecked: []string{}, Later: []string{}}
	rate := scrubRate()
	if b := s.scrubLimit.backlog(); b > copyCheckWork {
		reply.Later, reply.Wait = ids, (b - copyCheckWork).Seconds()
		return reply // busy with other scrubs: come back later
	}
	budget := int64(copyCheckWork.Seconds() * float64(rate))
	byNode := map[string][]string{}
	var planned int64
	for i, id := range ids {
		var node string
		var size int64
		if s.DB.QueryRow(`SELECT node_id, size FROM held_blocks WHERE owner = ? AND id = ?`, owner, id).Scan(&node, &size) != nil {
			reply.Missing = append(reply.Missing, id)
			continue
		}
		if planned > 0 && planned+size > budget {
			reply.Later = ids[i:]
			break
		}
		planned += size
		byNode[node] = append(byNode[node], id)
	}
	now := time.Now().Unix()
	for node, ids := range byNode {
		st := s.nodeStore(node)
		if st == nil {
			for _, id := range ids {
				s.DB.Exec(`DELETE FROM held_blocks WHERE owner = ? AND id = ?`, owner, id)
			}
			reply.Missing = append(reply.Missing, ids...)
			continue
		}
		for i := 0; i < len(ids); i += scrubBatch {
			chunk := ids[i:min(i+scrubBatch, len(ids))]
			res, err := s.verifyBlocks(ctx, st, owner, chunk, nonce)
			if err != nil {
				reply.Unchecked = append(reply.Unchecked, chunk...)
				continue
			}
			reply.Bytes += res.Bytes
			bad := map[string]bool{}
			for _, d := range res.Damaged {
				bad[d.ID] = true
				s.DB.Exec(`DELETE FROM held_blocks WHERE owner = ? AND id = ?`, owner, d.ID)
				if d.Missing {
					reply.Missing = append(reply.Missing, d.ID)
				} else {
					st.Delete(ctx, owner, d.ID)
					reply.Damaged = append(reply.Damaged, d.ID)
				}
			}
			for _, id := range chunk {
				if !bad[id] {
					reply.Answers[id] = res.Answers[id]
					s.DB.Exec(`UPDATE held_blocks SET verified = ? WHERE owner = ? AND id = ?`, now, owner, id)
				}
			}
		}
	}
	if n := len(reply.Damaged) + len(reply.Missing); n > 0 {
		s.log.Warn("copies kept for another site failed its scrub check; dropped", "owner", s.peerName(owner), "damaged", len(reply.Damaged), "missing", len(reply.Missing))
		s.notify(TopicReplication)
	}
	// Count the reads against the rate, but don't keep the caller waiting long:
	// a backlog makes the next request come back later instead.
	s.scrubLimit.reserve(reply.Bytes, rate)
	select {
	case <-ctx.Done():
	case <-time.After(min(s.scrubLimit.backlog(), copyCheckWork)):
	}
	return reply
}

// copyHolders lists, per site, which of the blocks it holds copies of.
func (s *Site) copyHolders(ids []string) map[string][]string {
	out := map[string][]string{}
	for _, id := range ids {
		rows, err := s.DB.Query(`SELECT site_id FROM replicas WHERE block_id = ? AND state = 'ok'`, id)
		if err != nil {
			continue
		}
		for rows.Next() {
			var sid string
			rows.Scan(&sid)
			out[sid] = append(out[sid], id)
		}
		rows.Close()
	}
	return out
}

type copyCheckResult struct {
	site  string
	ids   []string
	reply copyCheckReply
	err   error
}

// checkCopies asks every site holding copies of these blocks to check them,
// in parallel. Offline sites are skipped.
func (r *scrubRun) checkCopies(ctx context.Context, ids []string, nonce []byte) <-chan copyCheckResult {
	return r.checkSites(ctx, r.s.copyHolders(ids), nonce)
}

// checkSites asks each site to check the given copies (site -> block ids).
func (r *scrubRun) checkSites(ctx context.Context, holders map[string][]string, nonce []byte) <-chan copyCheckResult {
	s := r.s
	out := make(chan copyCheckResult)
	var wg sync.WaitGroup
	for site, list := range holders {
		var online bool
		s.DB.QueryRow(`SELECT online FROM peers WHERE site_id = ?`, site).Scan(&online)
		if !online {
			r.skip(site, list, "it was offline")
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A batch at a time; the other site checks what fits in a few
			// seconds and hands back the rest. A request that fails is tried
			// once more, then the site counts as unreachable for this batch.
			queue := list
			for len(queue) > 0 && ctx.Err() == nil {
				chunk := queue[:min(scrubBatch, len(queue))]
				queue = queue[len(chunk):]
				body, _ := json.Marshal(copyCheckRequest{Nonce: nonce, IDs: chunk})
				var reply copyCheckReply
				var err error
				for try := 0; try < 2; try++ {
					reply = copyCheckReply{}
					_, err = s.peerCall(ctx, site, http.MethodPost, "/peer/v1/scrub/check", body, &reply)
					if err == nil || ctx.Err() != nil {
						break
					}
				}
				if err != nil {
					// The rest isn't checked now either.
					out <- copyCheckResult{site, append(append([]string{}, chunk...), queue...), reply, err}
					return
				}
				out <- copyCheckResult{site, chunk, reply, err}
				if len(reply.Later) > 0 {
					queue = append(append([]string{}, reply.Later...), queue...)
					select {
					case <-ctx.Done():
					case <-time.After(time.Duration(reply.Wait * float64(time.Second))):
					}
				}
			}
		}()
	}
	go func() { wg.Wait(); close(out) }()
	return out
}

// compareCopies goes through what the sites holding copies answered, against
// this site's own answers (local: block id -> answer of the intact local copy).
func (r *scrubRun) compareCopies(ctx context.Context, results <-chan copyCheckResult, local map[string]string) {
	s := r.s
	for res := range results {
		name := s.peerName(res.site)
		if res.err != nil {
			if ctx.Err() == nil {
				r.skip(res.site, res.ids, "it couldn't be reached ("+res.err.Error()+")")
			}
			continue
		}
		var bad int64
		lost := func(id, problem, detail string) {
			bad++
			s.DB.Exec(`DELETE FROM replicas WHERE block_id = ? AND site_id = ?`, id, res.site)
			r.report(ScrubError{What: "remote", BlockID: id, Place: name, Problem: problem, Outcome: "replaced",
				Detail: detail + "; a new copy is being sent from this site"})
		}
		for _, id := range res.reply.Missing {
			lost(id, "missing", "the copy wasn't there")
		}
		for _, id := range res.reply.Damaged {
			lost(id, "damaged", "the copy didn't match its checksum and was dropped there")
		}
		var wrong []string
		for id, answer := range res.reply.Answers {
			// Without an answer here (this site's copy was damaged and just
			// repaired) the copy there counts as checked by its checksum.
			if mine, ok := local[id]; ok && answer != mine {
				wrong = append(wrong, id)
			}
		}
		if len(wrong) > 0 {
			s.peerDeleteBlocks(ctx, res.site, wrong)
			for _, id := range wrong {
				lost(id, "damaged", "the copy didn't pass the challenge: its bytes differ from this site's")
			}
		}
		checked := int64(len(res.reply.Answers) + len(res.reply.Missing) + len(res.reply.Damaged))
		s.DB.Exec(`INSERT INTO scrub_copies(scrub_id, site_id, checked, bad) VALUES(?, ?, ?, ?)
			ON CONFLICT(scrub_id, site_id) DO UPDATE SET checked = checked + excluded.checked, bad = bad + excluded.bad`,
			r.id, res.site, checked, bad)
		s.DB.Exec(`UPDATE scrubs SET copies = copies + ?, copy_bytes = copy_bytes + ? WHERE id = ?`, checked, res.reply.Bytes, r.id)
	}
}

// skip notes copies that couldn't be checked now, to check them at the end of
// the scrub (catchUpBatch), and reports the site once per scrub.
func (r *scrubRun) skip(site string, ids []string, why string) {
	if r.catchingUp {
		return // checked once more already; the report stays
	}
	for _, id := range ids {
		r.s.DB.Exec(`INSERT OR IGNORE INTO scrub_skipped(scrub_id, site_id, block_id) VALUES(?, ?, ?)`, r.id, site, id)
	}
	var reported int
	r.s.DB.QueryRow(`SELECT COUNT(*) FROM scrub_errors WHERE scrub_id = ? AND what = 'remote' AND problem = 'unreachable' AND block_id = ?`,
		r.id, site).Scan(&reported)
	if reported > 0 {
		return
	}
	// block_id holds the site's id, so the entry can be found again.
	r.report(ScrubError{What: "remote", BlockID: site, Place: r.s.peerName(site), Problem: "unreachable", Outcome: "skipped",
		Detail: why + ", so its copies of this site's blocks weren't checked. They're checked at the end of the scrub if it's back by then."})
}

// catchUpBatch checks copies that were skipped because their site was
// unreachable, on sites that are back. When a site's are all checked, its
// "unreachable" report goes. It returns the copies it checked (0: done).
func (r *scrubRun) catchUpBatch(ctx context.Context) int64 {
	s := r.s
	rows, err := s.DB.QueryContext(ctx, `SELECT k.site_id, k.block_id FROM scrub_skipped k JOIN peers p ON p.site_id = k.site_id
		WHERE k.scrub_id = ? AND p.online = 1 ORDER BY k.site_id, k.block_id LIMIT ?`, r.id, 8*scrubBatch)
	if err != nil {
		return 0
	}
	holders := map[string][]string{}
	var all []string
	var n int64
	for rows.Next() {
		var site, id string
		rows.Scan(&site, &id)
		holders[site] = append(holders[site], id)
		all = append(all, id)
		n++
	}
	rows.Close()
	if n == 0 {
		// Sites whose skipped copies are all checked: their report goes.
		s.DB.Exec(`DELETE FROM scrub_errors WHERE scrub_id = ? AND what = 'remote' AND problem = 'unreachable'
			AND block_id NOT IN (SELECT site_id FROM scrub_skipped WHERE scrub_id = ?)`, r.id, r.id)
		return 0
	}
	for site, ids := range holders {
		for _, id := range ids {
			s.DB.Exec(`DELETE FROM scrub_skipped WHERE scrub_id = ? AND site_id = ? AND block_id = ?`, r.id, site, id)
		}
	}
	// This site's answers for the same challenge, from its own copies.
	nonce := newNonce()
	local := map[string]string{}
	byNode := map[string][]string{}
	for _, id := range all {
		var node string
		if s.DB.QueryRow(`SELECT node_id FROM blocks WHERE id = ? AND refs > 0`, id).Scan(&node) == nil && node != "" {
			byNode[node] = append(byNode[node], id)
		}
	}
	remote := r.checkSites(ctx, holders, nonce)
	for node, ids := range byNode {
		if st := s.nodeStore(node); st != nil {
			for i := 0; i < len(ids); i += scrubBatch {
				if res, err := s.verifyBlocks(ctx, st, blockstore.OwnNS, ids[i:min(i+scrubBatch, len(ids))], nonce); err == nil {
					for id, a := range res.Answers {
						local[id] = a
					}
				}
			}
		}
	}
	r.catchingUp = true
	r.compareCopies(ctx, remote, local)
	r.catchingUp = false
	return n
}

func newNonce() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}
