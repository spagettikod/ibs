package db

import "testing"

// Going back is blocked only by changes that break older versions.
func TestBreakingSince(t *testing.T) {
	var breaking int = -1
	for i, m := range migrations {
		if m.Breaks != "" {
			breaking = i
			break
		}
	}
	if breaking < 0 {
		t.Fatal("no breaking change to test with")
	}
	if why := BreakingSince(breaking+1, Level); why != "" && breaking+1 < Level && migrations[breaking+1].Breaks == "" {
		t.Fatalf("a version that knows the breaking change is blocked: %s", why)
	}
	if BreakingSince(breaking, Level) == "" {
		t.Fatal("a version from before a breaking change isn't blocked")
	}
	if BreakingSince(breaking-1, breaking) != "" {
		t.Fatal("changes that don't break older versions block them")
	}
	if BreakingSince(Level, Level) != "" {
		t.Fatal("the current level blocks itself")
	}
}
