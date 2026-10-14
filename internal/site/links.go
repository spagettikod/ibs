package site

import (
	"sort"
	"strings"
	"time"
)

// Link is one connection in the IBS as reported by the site at its From
// end: to another site (Kind quic, direct or relay) or to a relay (Kind
// uplink, To = "relay:<id>"). Sites share links in their status, and pass on
// what they learned, so every site can draw the whole network.
type Link struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	Kind      string  `json:"kind"`
	Relay     string  `json:"relay,omitempty"`
	RelayName string  `json:"relay_name,omitempty"`
	RTTMillis float64 `json:"rtt_ms"`
	Bps       float64 `json:"bps"`
	Online    bool    `json:"online"`
	Updated   int64   `json:"updated"`
}

const (
	LinkUplink = "uplink"
	linkMaxAge = 10 * time.Minute
)

func linkKey(l Link) string { return l.From + ">" + l.To }

// ownLinks reports this site's links as of now.
func (s *Site) ownLinks() []Link {
	now := time.Now().Unix()
	var out []Link
	rows, err := s.DB.Query(`SELECT site_id, online FROM peers`)
	if err == nil {
		type p struct {
			id     string
			online bool
		}
		var peers []p
		for rows.Next() {
			var x p
			rows.Scan(&x.id, &x.online)
			peers = append(peers, x)
		}
		rows.Close()
		for _, x := range peers {
			pi := s.pathInfo(x.id)
			out = append(out, Link{From: s.ID.SiteID, To: x.id, Kind: pi.Kind, Relay: pi.Relay, RelayName: pi.RelayName,
				RTTMillis: pi.RTTMillis, Bps: pi.Bps, Online: x.online && pi.Kind != "", Updated: now})
		}
	}
	for _, r := range s.Relays() {
		out = append(out, Link{From: s.ID.SiteID, To: "relay:" + r.ID, Kind: LinkUplink, Relay: r.ID, RelayName: r.Name,
			RTTMillis: r.RTTMillis, Online: r.Connected, Updated: now})
	}
	return out
}

// learnLinks merges links reported by a peer, keeping the newest per link.
// This site is the only authority on its own links.
func (s *Site) learnLinks(links []Link) {
	cutoff := time.Now().Add(-linkMaxAge).Unix()
	s.linksMu.Lock()
	defer s.linksMu.Unlock()
	for _, l := range links {
		if l.From == s.ID.SiteID || l.Updated < cutoff || l.From == "" || l.To == "" {
			continue
		}
		if old, ok := s.links[linkKey(l)]; !ok || old.Updated < l.Updated {
			s.links[linkKey(l)] = l
		}
	}
	for k, l := range s.links {
		if l.Updated < cutoff {
			delete(s.links, k)
		}
	}
}

// NetworkLinks returns every known link in the IBS: this site's own and those
// learned from others.
func (s *Site) NetworkLinks() []Link {
	out := s.ownLinks()
	cutoff := time.Now().Add(-linkMaxAge).Unix()
	s.linksMu.Lock()
	for _, l := range s.links {
		if l.Updated >= cutoff {
			out = append(out, l)
		}
	}
	s.linksMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return linkKey(out[i]) < linkKey(out[j]) })
	return out
}

// RelayNode is a relay that appears in the network view.
type RelayNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func relayNodes(links []Link) []RelayNode {
	seen := map[string]string{}
	for _, l := range links {
		if l.Relay != "" && (l.Kind == LinkUplink || l.Kind == PathRelay) {
			if seen[l.Relay] == "" {
				seen[l.Relay] = l.RelayName
			}
		}
		if id, ok := strings.CutPrefix(l.To, "relay:"); ok && seen[id] == "" {
			seen[id] = l.RelayName
		}
	}
	out := []RelayNode{}
	for id, name := range seen {
		out = append(out, RelayNode{ID: id, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
