package site

import (
	"context"
	"encoding/json"
	"time"

	"ibs/internal/relay"
)

// followRelayMoves keeps relay addresses current across the IBS, so after a
// relay moves or is renamed its new address only has to be set on one site:
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
	rows, err := s.DB.Query(`SELECT relay_tokens, relay_addresses FROM peers WHERE online = 1`)
	if err != nil {
		return
	}
	var announced []relay.Token
	newest := map[string]RelayAddress{} // relay id -> the most recently set address reported
	for rows.Next() {
		var tj, aj string
		rows.Scan(&tj, &aj)
		var toks []string
		json.Unmarshal([]byte(tj), &toks)
		for _, tok := range toks {
			if t, err := relay.ParseToken(tok); err == nil && t.Address != "" {
				announced = append(announced, t)
			}
		}
		var addrs []RelayAddress
		json.Unmarshal([]byte(aj), &addrs)
		for _, a := range addrs {
			if a.Address != "" && a.Set > newest[a.Relay].Set {
				newest[a.Relay] = a
			}
		}
	}
	rows.Close()

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
