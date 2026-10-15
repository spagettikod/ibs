package site

import (
	"context"
	"encoding/json"
	"time"

	"ibs/internal/relay"
)

// followRelayMoves keeps relay addresses current across the IBS: when a relay
// this site is registered with can't be reached, and a site it's connected to
// reports the same relay (by its key) at another address, this site tries
// that address. So after a relay moves, its new address only has to be set on
// one site. The relay's key is pinned, so a wrong address just doesn't
// connect; each address is tried at most every couple of minutes.
func (s *Site) followRelayMoves(context.Context) {
	rows, err := s.DB.Query(`SELECT relay_tokens FROM peers WHERE online = 1`)
	if err != nil {
		return
	}
	var announced []relay.Token
	for rows.Next() {
		var tj string
		rows.Scan(&tj)
		var toks []string
		json.Unmarshal([]byte(tj), &toks)
		for _, tok := range toks {
			if t, err := relay.ParseToken(tok); err == nil && t.Address != "" {
				announced = append(announced, t)
			}
		}
	}
	rows.Close()
	for _, t := range announced {
		rc := s.relayClient(relay.ID(t.Key))
		if rc == nil || rc.Connected() || rc.Token.Address == t.Address || rc.Status().Error == "" {
			continue // not ours, working, the same address, or still connecting
		}
		key := relay.ID(t.Key) + " " + t.Address
		s.netMu.Lock()
		if time.Since(s.relayMoveTried[key]) < 2*time.Minute {
			s.netMu.Unlock()
			continue
		}
		s.relayMoveTried[key] = time.Now()
		s.netMu.Unlock()
		s.log.Info("relay can't be reached; trying the address another site uses", "relay", rc.Token.Address, "new", t.Address)
		if _, err := s.AddRelay("", t.EncodeWithAddress()); err != nil {
			s.log.Warn("use relay's new address", "relay", t.Address, "err", err)
		}
	}
}
