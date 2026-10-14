package site

import "testing"

func TestSetLocation(t *testing.T) {
	s := newTestSite(t)
	got, err := s.SetLocation(&Location{59.329312, 18.068581})
	if err != nil {
		t.Fatal(err)
	}
	if *got != (Location{59.3293, 18.0686}) {
		t.Fatalf("stored %v, want the clicked spot", got)
	}
	if l := s.Location(); l == nil || *l != *got || s.Status().Location == nil {
		t.Fatalf("stored location: %v", l)
	}
	if _, err := s.SetLocation(&Location{91, 0}); err == nil {
		t.Fatal("invalid location accepted")
	}
	s.SetLocation(nil)
	if s.Location() != nil || s.Status().Location != nil {
		t.Fatal("location not removed")
	}
}
