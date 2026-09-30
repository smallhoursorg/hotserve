package main

import (
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/backups/record"
)

// Whether a drill made a check is read off the record — the last check
// before and after — and never off the clock, which a box that has just
// booted may step back while the drill runs.
func TestADrillSaysTheCheckItMade(t *testing.T) {
	at := time.Date(2026, 9, 27, 3, 30, 0, 0, time.UTC)
	before := &record.Check{Time: at, Group: "39/52", Class: record.CheckClean}
	for name, tc := range map[string]struct {
		before, after *record.Check
		made          bool
	}{
		"the first":              {after: &record.Check{Time: at, Group: "40/52", Class: record.CheckDamaged}, made: true},
		"a new one":              {before: before, after: &record.Check{Time: at.Add(7 * 24 * time.Hour), Group: "40/52", Class: record.CheckDamaged}, made: true},
		"the clock stepped back": {before: before, after: &record.Check{Time: at.Add(-time.Hour), Group: "40/52", Class: record.CheckDamaged}, made: true},
		"the same one":           {before: before, after: &record.Check{Time: at, Group: "39/52", Class: record.CheckClean}},
		"none":                   {before: before},
		"none before or after":   {},
	} {
		if got := newCheck(tc.before, tc.after); got != tc.made {
			t.Errorf("%s: made %v, want %v", name, got, tc.made)
		}
	}
}
