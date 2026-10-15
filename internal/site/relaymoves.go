package site

import (
	"context"
	"encoding/json"
	"time"

	"ibs/internal/relay"
)

// followRelayMoves keeps relays the same across the IBS: relays added or
// removed on one site are added or removed on the others (followRelayChanges),
// and after a relay moves or is renamed its new address only has to be set on
// one site:
//
//   - Renamed: a connected site set an address for the relay by hand more
//     recently than this site did. This site takes it, keeping when it was
//     set, so it spreads further and two addresses can't take turns. If the
//     relay doesn't connect at the new address, this site goes back to the
//     old one and tries again later.
//   - Moved: the relay can't be reached here, and a connected site reports it
//     at another address. This site tries that one.
//
// The relay's key is pinned, so a wrong address just doesn't connect.
func (s *Site) followRelayMoves(context.Context) {
	rows, err := s.DB.Query(`SELECT relay_tokens, relay_addresses, relay_removals FROM peers WHERE online = 1`)
	if err != nil {
		return
	}
	var announced []relay.Token
	newest := map[string]RelayAddress{} // relay id -> the most recently set address reported
	added := map[string]RelayAddress{}  // relay id -> when a site reports it added, latest
	removed := map[string]int64{}       // relay id -> when it was removed on some site, latest
	tokens := map[string]relay.Token{}  // relay id -> a token to register with it
	for rows.Next() {
		var tj, aj, rj string
		rows.Scan(&tj, &aj, &rj)
		var toks []string
		json.Unmarshal([]byte(tj), &toks)
		for _, tok := range toks {
			if t, err := relay.ParseToken(tok); err == nil && t.Address != "" {
				announced = append(announced, t)
				tokens[relay.ID(t.Key)] = t
			}
		}
		var addrs []RelayAddress
		json.Unmarshal([]byte(aj), &addrs)
		for _, a := range addrs {
			if a.Address != "" && a.Set > newest[a.Relay].Set {
				newest[a.Relay] = a
			}
			if a.Added > added[a.Relay].Added {
				added[a.Relay] = a
			}
		}
		var rms []RelayRemoval
		json.Unmarshal([]byte(rj), &rms)
		for _, r := range rms {
			removed[r.Relay] = max(removed[r.Relay], r.At)
		}
	}
	rows.Close()
	s.followRelayChanges(added, removed, tokens)

	for id, a := range newest {
		var addr string
		var set int64
		if s.DB.QueryRow(`SELECT address, address_set FROM relays WHERE id = ?`, id).Scan(&addr, &set) != nil ||
			addr == a.Address || set >= a.Set || !s.tryRelayAddress(id, a.Address) {
			continue
		}
		s.log.Info("relay has a new address, set on another site", "relay", addr, "new", a.Address)
		if err := s.setRelayAddress(id, a.Address, a.Set); err != nil {
			continue
		}
		if s.waitRelay(id, 30*time.Second) {
			continue
		}
		s.log.Warn("relay doesn't connect at its new address; keeping the old one for now", "relay", addr, "new", a.Address)
		s.setRelayAddress(id, addr, set)
	}

	for _, t := range announced {
		rc := s.relayClient(relay.ID(t.Key))
		if rc == nil || rc.Connected() || rc.Token.Address == t.Address || rc.Status().Error == "" {
			continue // not ours, working, the same address, or still connecting
		}
		if !s.tryRelayAddress(relay.ID(t.Key), t.Address) {
			continue
		}
		s.log.Info("relay can't be reached; trying the address another site uses", "relay", rc.Token.Address, "new", t.Address)
		if _, err := s.AddRelay("", t.EncodeWithAddress()); err != nil {
			s.log.Warn("use relay's new address", "relay", t.Address, "err", err)
		}
	}
}

// followRelayChanges adds relays other sites added and removes relays other
// sites removed, so the IBS uses the same relays without setting each site.
// For each relay the latest of adding and removing it wins, by when it
// happened on the site where it was done by hand. A relay is not removed when
// that would leave this site with no way for other sites to reach it.
func (s *Site) followRelayChanges(added map[string]RelayAddress, removed map[string]int64, tokens map[string]relay.Token) {
	have := map[string]int64{} // relay id -> when added
	rows, err := s.DB.Query(`SELECT id, added FROM relays`)
	if err != nil {
		return
	}
	for rows.Next() {
		var id string
		var at int64
		rows.Scan(&id, &at)
		have[id] = at
	}
	rows.Close()
	gone := map[string]int64{} // relay id -> when removed here
	if rows, err := s.DB.Query(`SELECT id, at FROM removed_relays`); err == nil {
		for rows.Next() {
			var id string
			var at int64
			rows.Scan(&id, &at)
			gone[id] = at
		}
		rows.Close()
	}

	for id, at := range removed {
		since, ok := have[id]
		if !ok || since >= at {
			if !ok && gone[id] < at {
				s.DB.Exec(`INSERT INTO removed_relays(id, at) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET at = max(at, excluded.at)`, id, at)
			}
			continue // not used here, or added again since
		}
		name := id[:8]
		if rc := s.relayClient(id); rc != nil {
			name = rc.Token.Address
		}
		if len(have) == 1 && s.Setting("peer_advertise") == "" {
			if s.tryRelayAddress(id, "keep") {
				s.log.Warn("another site removed a relay, but it's this site's only way to be reached; keeping it", "relay", name)
			}
			continue
		}
		if s.removeRelay(id, at) == nil {
			delete(have, id)
			s.log.Info("relay removed on another site; not using it any more", "relay", name)
		}
	}

	for id, a := range added {
		t, ok := tokens[id]
		if _, mine := have[id]; mine || !ok || a.Added == 0 || a.Added <= max(gone[id], removed[id]) {
			continue
		}
		if !s.tryRelayAddress(id, "add") {
			continue
		}
		if _, err := s.addRelay(a.Address, t.EncodeWithAddress(), a.Added); err != nil {
			s.log.Warn("use relay added on another site", "relay", a.Address, "err", err)
			continue
		}
		s.log.Info("relay added on another site; using it too", "relay", a.Address)
	}
}

// tryRelayAddress reports whether a relay may be tried at an address now: at
// most every couple of minutes.
func (s *Site) tryRelayAddress(id, address string) bool {
	key := id + " " + address
	s.netMu.Lock()
	defer s.netMu.Unlock()
	if time.Since(s.relayMoveTried[key]) < 2*time.Minute {
		return false
	}
	s.relayMoveTried[key] = time.Now()
	return true
}
