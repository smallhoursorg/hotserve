// Fuzz target for the hint-header parser — the one input attackers
// fully control on every request. Property: whatever arrives, the
// parsed level is always within the valid range (garbage degrades to
// the harmless level 1, never to a panic or an out-of-range value).
package penaltybox

import "testing"

func FuzzParseLevel(f *testing.F) {
	for _, s := range []string{"", "1", "2", "3", "0", "4", "10", "-1", "03", " 2", "2 ", "banana", "2;q=1", "\x00", "999999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		for _, vals := range [][]string{{v}, {v, v}, {"2", v}} {
			got := parseLevel(vals)
			if got < 1 || got > 3 {
				t.Errorf("parseLevel(%q) = %d, outside [1,3]", vals, got)
			}
		}
	})
}
