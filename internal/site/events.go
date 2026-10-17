package site

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Topics announced to admin UI subscribers when state changes.
const (
	TopicData        = "data"        // buckets, objects, keys, settings
	TopicPeers       = "peers"       // pairing, peer status, the known universe
	TopicNodes       = "nodes"       // storage nodes
	TopicReplication = "replication" // replica counts, held blocks, healing
	// TopicScrub ("scrub", scrub.go): scrub progress and findings.
)

// coalesceDelay batches bursts (e.g. a multi-block upload) into one event.
// During a long stream of changes (many uploads) events go out at most every
// eventInterval: each makes the web interface reload pages whose queries
// compete with the uploads for the disk.
const (
	coalesceDelay = 150 * time.Millisecond
	eventInterval = 2 * time.Second
)

// hub fans change notifications out to subscribers. Topics raised close
// together are merged so subscribers see at most one event per delay.
type hub struct {
	mu      sync.Mutex
	subs    map[chan []string]struct{}
	pending map[string]bool
	timer   *time.Timer
	last    time.Time // when events last went out
}

func newHub() *hub {
	return &hub{subs: map[chan []string]struct{}{}, pending: map[string]bool{}}
}

func (h *hub) notify(topics ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return
	}
	for _, t := range topics {
		h.pending[t] = true
	}
	if h.timer == nil {
		h.timer = time.AfterFunc(max(coalesceDelay, eventInterval-time.Since(h.last)), h.flush)
	}
}

// notifyNow sends topics within coalesceDelay, also during a stream of
// changes: for what the user should see at once, like activity starting and
// stopping.
func (h *hub) notifyNow(topics ...string) {
	h.mu.Lock()
	for _, t := range topics {
		h.pending[t] = true
	}
	if h.timer != nil {
		h.timer.Stop()
	}
	h.timer = time.AfterFunc(coalesceDelay, h.flush)
	h.mu.Unlock()
}

func (h *hub) flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.timer = nil
	h.last = time.Now()
	topics := make([]string, 0, len(h.pending))
	for t := range h.pending {
		topics = append(topics, t)
	}
	sort.Strings(topics)
	clear(h.pending)
	for ch := range h.subs {
		select {
		case ch <- topics:
		default: // subscriber is behind; it will catch up on the next event
		}
	}
}

// Subscribe returns a channel of topic batches until ctx is done.
func (s *Site) Subscribe(ctx context.Context) <-chan []string {
	ch := make(chan []string, 16)
	s.events.mu.Lock()
	s.events.subs[ch] = struct{}{}
	s.events.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.events.mu.Lock()
		delete(s.events.subs, ch)
		s.events.mu.Unlock()
	}()
	return ch
}

// notify tells the web interface that something changed, and the background
// work that depends on what's stored: cached figures and replication scans.
func (s *Site) notify(topics ...string) {
	s.statsStale.Store(true)
	s.replDirty.Store(true)
	s.events.notify(topics...)
}

// watchState raises events for changes that happen by the passage of time
// rather than by an action, such as a storage node missing its heartbeats.
func (s *Site) watchState(context.Context) {
	cutoff := time.Now().Add(-nodeTimeout).Unix()
	rows, err := s.DB.Query(`SELECT id, last_seen >= ? FROM nodes ORDER BY id`, cutoff)
	if err != nil {
		return
	}
	var b strings.Builder
	for rows.Next() {
		var id string
		var online bool
		rows.Scan(&id, &online)
		b.WriteString(id)
		if online {
			b.WriteByte('+')
		}
		b.WriteByte(',')
	}
	rows.Close()
	b.WriteByte('|')
	for _, q := range []string{
		`SELECT site_id || name || online || (free >> 30) FROM peers ORDER BY site_id`,
		`SELECT site_id || name || online || (free >> 30) FROM network ORDER BY site_id`,
	} {
		if r, err := s.DB.Query(q); err == nil {
			for r.Next() {
				var v string
				r.Scan(&v)
				b.WriteString(v)
				b.WriteByte(',')
			}
			r.Close()
		}
	}
	h := ReplicationHealth{}
	s.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE refs > 0 AND node_id = ''`).Scan(&h.MissingLocally)
	fp := b.String() + "|" + itoa(h.MissingLocally)
	s.peerMu.Lock()
	changed := fp != s.lastState
	s.lastState = fp
	s.peerMu.Unlock()
	if changed {
		s.notify(TopicNodes, TopicPeers, TopicReplication)
	}
}
