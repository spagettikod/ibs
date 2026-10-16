package site

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
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
// The site holding the copies reads them at its own scrub rate, whoever asks.
// It still checks copies itself that their owner hasn't checked for a while
// (heldSelfCheckAfter), e.g. of a site that has been offline for long.

const (
	heldSelfCheckAfter = 60 * 24 * time.Hour
	copyCheckTimeout   = 5 * time.Minute // per request of scrubBatch blocks
)

// byteLimiter keeps reads to a rate across everything that reads for scrubs.
type byteLimiter struct {
	mu   sync.Mutex
	next time.Time
}

// wait returns when n more bytes fit in the rate.
func (l *byteLimiter) wait(ctx context.Context, n, rate int64) {
	d := time.Duration(float64(n) / float64(rate) * float64(time.Second))
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(d)
	until := l.next
	l.mu.Unlock()
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
	reply := copyCheckReply{Answers: map[string]string{}, Damaged: []string{}, Missing: []string{}, Unchecked: []string{}}
	byNode := map[string][]string{}
	for _, id := range ids {
		var node string
		if s.DB.QueryRow(`SELECT node_id FROM held_blocks WHERE owner = ? AND id = ?`, owner, id).Scan(&node) != nil {
			reply.Missing = append(reply.Missing, id)
			continue
		}
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
	s.scrubLimit.wait(ctx, reply.Bytes, scrubRate())
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
	s := r.s
	out := make(chan copyCheckResult)
	holders := s.copyHolders(ids)
	var wg sync.WaitGroup
	for site, list := range holders {
		var online bool
		s.DB.QueryRow(`SELECT online FROM peers WHERE site_id = ?`, site).Scan(&online)
		if !online {
			r.siteSkipped(site, "it was offline, so its copies of this site's blocks weren't checked")
			continue
		}
		if r.legacy[site] {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A few blocks per request, each with a time limit and one retry,
			// so a connection that hangs costs minutes, not the scrub.
			for i := 0; i < len(list); i += scrubBatch {
				chunk := list[i:min(i+scrubBatch, len(list))]
				body, _ := json.Marshal(copyCheckRequest{Nonce: nonce, IDs: chunk})
				var reply copyCheckReply
				var err error
				for try := 0; try < 2; try++ {
					reply = copyCheckReply{}
					_, err = s.peerCall(longCall(ctx, copyCheckTimeout), site, http.MethodPost, "/peer/v1/scrub/check", body, &reply)
					if err == nil || isStatus(err, http.StatusNotFound) || ctx.Err() != nil {
						break
					}
				}
				out <- copyCheckResult{site, chunk, reply, err}
				if err != nil {
					return
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
			if isStatus(res.err, http.StatusNotFound) {
				r.legacy[res.site] = true
				r.siteSkipped(res.site, "it runs an older version that checks the copies itself; see Your copies on other sites")
			} else if ctx.Err() == nil {
				r.siteSkipped(res.site, "it couldn't be reached, so its copies of this site's blocks weren't checked: "+res.err.Error())
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
			// No answer to compare (an older node, or this site's copy was
			// just repaired): the copy matched its checksum there.
			if mine, ok := local[id]; ok && answer != "" && answer != mine {
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

// siteSkipped reports once per scrub that a site's copies weren't checked.
func (r *scrubRun) siteSkipped(site, detail string) {
	if r.skipped[site] {
		return
	}
	r.skipped[site] = true
	r.report(ScrubError{What: "remote", Place: r.s.peerName(site), Problem: "unreachable", Outcome: "skipped", Detail: detail})
}

func newNonce() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}
