package site

import (
	"sync"
	"time"
)

// Files arriving and being deleted, per bucket, so the web interface can show
// that something is happening while the counted figures (stats.go) catch up.
// A bucket counts as busy until activityWindow after its last change.

const activityWindow = time.Second

type activity struct {
	mu      sync.Mutex
	writes  map[string]time.Time // bucket -> last file stored
	deletes map[string]time.Time // bucket -> last file deleted
	was     bool                 // busy at the last check (activityEvents)
}

// Activity is what's happening now: per bucket "writing" or "deleting".
type Activity struct {
	Writing  bool              `json:"writing"`
	Deleting bool              `json:"deleting"`
	Buckets  map[string]string `json:"buckets"`
}

func (s *Site) noteWrite(bucket string)  { s.noteActivity(bucket, false) }
func (s *Site) noteDelete(bucket string) { s.noteActivity(bucket, true) }

func (s *Site) noteActivity(bucket string, deleted bool) {
	a := &s.act
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writes == nil {
		a.writes, a.deletes = map[string]time.Time{}, map[string]time.Time{}
	}
	if deleted {
		a.deletes[bucket] = time.Now()
	} else {
		a.writes[bucket] = time.Now()
	}
}

// Activity reports the buckets with files arriving or being deleted.
func (s *Site) Activity() Activity {
	a := &s.act
	out := Activity{Buckets: map[string]string{}}
	cutoff := time.Now().Add(-activityWindow)
	a.mu.Lock()
	defer a.mu.Unlock()
	for b, t := range a.deletes {
		if t.After(cutoff) {
			out.Deleting, out.Buckets[b] = true, "deleting"
		} else {
			delete(a.deletes, b)
		}
	}
	for b, t := range a.writes {
		if t.After(cutoff) {
			out.Writing, out.Buckets[b] = true, "writing"
		} else {
			delete(a.writes, b)
		}
	}
	return out
}

// activityEvents tells the web interface when activity starts and stops, so
// the spinners come and go; it runs every half second.
func (s *Site) activityEvents() {
	act := s.Activity()
	busy := act.Writing || act.Deleting
	s.act.mu.Lock()
	changed := s.act.was != busy
	s.act.was = busy
	s.act.mu.Unlock()
	if changed {
		s.events.notifyNow(TopicData)
	}
}
