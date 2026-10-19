package site

import (
	"context"
	"sync"
	"time"
)

// Copy traffic per site, so the web interface can show that copies are on
// their way and how fast: bytes of copies sent to each site (our blocks) and
// received from it (its blocks), over the last rateWindow seconds.

const rateWindow = 10

type rateMeter struct {
	mu    sync.Mutex
	bytes [rateWindow]int64
	at    [rateWindow]int64 // the second each slot counts
}

func (m *rateMeter) add(n int64) {
	now := time.Now().Unix()
	i := now % rateWindow
	m.mu.Lock()
	if m.at[i] != now {
		m.at[i], m.bytes[i] = now, 0
	}
	m.bytes[i] += n
	m.mu.Unlock()
}

// bps is the average over the last rateWindow seconds.
func (m *rateMeter) bps() float64 {
	now := time.Now().Unix()
	var sum int64
	m.mu.Lock()
	for i := range m.bytes {
		if now-m.at[i] < rateWindow {
			sum += m.bytes[i]
		}
	}
	m.mu.Unlock()
	return float64(sum) / rateWindow
}

// out: our blocks' copies sent to the site; in: its copies arriving here; back:
// our own blocks fetched back from it after a restore.
type peerRates struct{ out, in, back rateMeter }

func (s *Site) peerRates(siteID string) *peerRates {
	r, _ := s.rates.LoadOrStore(siteID, &peerRates{})
	return r.(*peerRates)
}

// sentCopy and receivedCopy count a copy sent to or received from a site.
func (s *Site) sentCopy(siteID string, n int)     { s.peerRates(siteID).out.add(int64(n)) }
func (s *Site) receivedCopy(siteID string, n int) { s.peerRates(siteID).in.add(int64(n)) }

// restoreRate is how fast this site's own blocks come back from a site.
func (s *Site) restoreRate(siteID string) float64 {
	if r, ok := s.rates.Load(siteID); ok {
		return r.(*peerRates).back.bps()
	}
	return 0
}

// copyRates returns how fast copies go to (out) and come from (in) a site.
func (s *Site) copyRates(siteID string) (out, in float64) {
	r, ok := s.rates.Load(siteID)
	if !ok {
		return 0, 0
	}
	return r.(*peerRates).out.bps(), r.(*peerRates).in.bps()
}

// transferEvents tells the web interface to refresh while copies move, and
// once more after they stopped, so the speeds shown go back to zero.
func (s *Site) transferEvents(context.Context) {
	active := s.restoring.bps() > 0 // blocks fetched back after a restore
	s.rates.Range(func(_, v any) bool {
		r := v.(*peerRates)
		active = active || r.out.bps() > 0 || r.in.bps() > 0 || r.back.bps() > 0
		return !active
	})
	if active || s.wasMoving {
		s.notify(TopicReplication)
	}
	s.wasMoving = active
}
