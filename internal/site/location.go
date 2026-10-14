package site

import (
	"encoding/json"
	"errors"
	"math"

	"ibs/internal/db"
)

// Location is where a site is shown on the map: the spot its admin clicked,
// meant to be a general area rather than an address.
type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

func (l *Location) valid() bool {
	return l != nil && l.Lat >= -90 && l.Lat <= 90 && l.Lon >= -180 && l.Lon <= 180 && !math.IsNaN(l.Lat) && !math.IsNaN(l.Lon)
}

// round keeps a location to about 10 m.
func round(p Location) Location {
	r := func(v float64) float64 { return math.Round(v*10000) / 10000 }
	return Location{r(p.Lat), r(p.Lon)}
}

// Location returns where this site is shown on the map, or nil.
func (s *Site) Location() *Location {
	var l Location
	if json.Unmarshal([]byte(s.Setting("location")), &l) != nil || !l.valid() {
		return nil
	}
	return &l
}

// SetLocation stores where this site is shown on the map. nil removes it.
func (s *Site) SetLocation(pin *Location) (*Location, error) {
	if pin == nil {
		db.SetSetting(s.DB, "location", "")
		s.markDirty()
		s.notify(TopicPeers)
		return nil, nil
	}
	if !pin.valid() {
		return nil, errors.New("invalid location")
	}
	l := round(*pin)
	b, _ := json.Marshal(l)
	if err := db.SetSetting(s.DB, "location", string(b)); err != nil {
		return nil, err
	}
	s.markDirty()
	s.notify(TopicPeers)
	return &l, nil
}

func locationJSON(l *Location) string {
	if !l.valid() {
		return ""
	}
	b, _ := json.Marshal(l)
	return string(b)
}

func parseLocation(s string) *Location {
	var l Location
	if s == "" || json.Unmarshal([]byte(s), &l) != nil || !l.valid() {
		return nil
	}
	return &l
}
