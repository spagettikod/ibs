package site

import (
	"context"
	"time"
)

// Figures that take a pass over every block (copy counts, per site, node and
// bucket) are kept in a cache by one background routine (statsLoop), so pages
// read them at once however many files a site has. The routine recalculates
// when something changed, at most every statsMinAge, and at least every
// statsMaxAge for what changes with time (sites going offline). Until the
// first calculation after a start, pages show the figures as not counted yet.
const (
	statsMinAge = 5 * time.Second
	statsMaxAge = time.Minute
)

// Stats is a snapshot of the expensive figures.
type Stats struct {
	At          int64
	Health      ReplicationHealth
	Capacity    IBSCapacity
	Objects     int64
	ObjectBytes int64
	HeldBlocks  int64
	HeldBytes   int64
	HeldOffline int64 // of the held bytes, copies of sites that are offline
	Peers       map[string]PeerCounts
	Nodes       map[string]NodeCounts
	Buckets     map[string]BucketCounts
}

type PeerCounts struct{ ReplicasOut, BytesOut, Removing, HeldBlocks, HeldBytes int64 }
type NodeCounts struct{ Blocks, HeldBlocks, Used, Stranded int64 }
type BucketCounts struct{ Objects, Size, Pending int64 }

// stats returns the cached figures, or nil before the first calculation.
// Without statsLoop running (in tests) it calculates them now.
func (s *Site) stats() *Stats {
	if !s.statsRunning.Load() {
		s.refreshStats()
	}
	return s.statsCache.Load()
}

// statsLoop keeps the cache up to date.
func (s *Site) statsLoop(ctx context.Context) {
	s.statsRunning.Store(true)
	var took time.Duration // the last refresh
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		st := s.statsCache.Load()
		// Refresh no more often than five times as long as a refresh takes,
		// so it never keeps the database busy more than a fifth of the time.
		minAge := max(statsMinAge, 5*took)
		if st == nil || time.Since(time.Unix(0, st.At)) >= max(statsMaxAge, minAge) ||
			s.statsStale.Load() && time.Since(time.Unix(0, st.At)) >= minAge {
			s.statsStale.Store(false)
			start := time.Now()
			s.refreshStats()
			took = time.Since(start)
			// Pages showing the figures reload; not through s.notify, which
			// would mark them stale again.
			s.events.notify(TopicReplication, TopicData, TopicNodes, TopicPeers)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refreshStats calculates the figures and caches them.
func (s *Site) refreshStats() {
	st := &Stats{At: time.Now().UnixNano(), Peers: map[string]PeerCounts{}, Nodes: map[string]NodeCounts{}, Buckets: map[string]BucketCounts{}}
	var groups []blockGroup
	st.Health, groups = s.blockFigures()
	st.Capacity = s.IBSCapacity(st.Health)
	for _, g := range groups {
		b := st.Buckets[g.bucket]
		b.Pending += g.under
		st.Buckets[g.bucket] = b
		n := st.Nodes[g.node]
		n.Blocks += g.blocks
		n.Used += g.bytes
		n.Stranded += g.stranded
		st.Nodes[g.node] = n
	}

	if rows, err := s.DB.Query(`SELECT r.site_id, SUM(r.state = 'ok'), COALESCE(SUM(CASE WHEN r.state = 'ok' THEN b.size END), 0),
			SUM(r.state = 'delete') FROM replicas r LEFT JOIN blocks b ON b.id = r.block_id GROUP BY r.site_id`); err == nil {
		for rows.Next() {
			var id string
			var c PeerCounts
			rows.Scan(&id, &c.ReplicasOut, &c.BytesOut, &c.Removing)
			st.Peers[id] = c
		}
		rows.Close()
	}
	if rows, err := s.DB.Query(`SELECT owner, COUNT(*), SUM(size) FROM held_blocks GROUP BY owner`); err == nil {
		for rows.Next() {
			var id string
			var n, b int64
			rows.Scan(&id, &n, &b)
			c := st.Peers[id]
			c.HeldBlocks, c.HeldBytes = n, b
			st.Peers[id] = c
			st.HeldBlocks += n
			st.HeldBytes += b
		}
		rows.Close()
	}
	s.DB.QueryRow(`SELECT COALESCE(SUM(h.size), 0) FROM held_blocks h
		WHERE h.owner NOT IN (SELECT site_id FROM peers WHERE online = 1)`).Scan(&st.HeldOffline)

	nodeRows := func(q string, set func(c *NodeCounts, a, b int64)) {
		rows, err := s.DB.Query(q)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var a, b int64
			rows.Scan(&id, &a, &b)
			c := st.Nodes[id]
			set(&c, a, b)
			st.Nodes[id] = c
		}
	}
	nodeRows(`SELECT node_id, COUNT(*), SUM(size) FROM held_blocks GROUP BY node_id`,
		func(c *NodeCounts, a, b int64) { c.HeldBlocks = a; c.Used += b })
	if rows, err := s.DB.Query(`SELECT bucket, COUNT(*), SUM(size) FROM objects GROUP BY bucket`); err == nil {
		for rows.Next() {
			var name string
			c := st.Buckets[name]
			rows.Scan(&name, &c.Objects, &c.Size)
			c.Pending = st.Buckets[name].Pending
			st.Buckets[name] = c
			st.Objects += c.Objects
			st.ObjectBytes += c.Size
		}
		rows.Close()
	}
	s.statsCache.Store(st)
}
