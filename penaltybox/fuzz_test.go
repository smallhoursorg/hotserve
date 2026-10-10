// Fuzz targets for the inputs a client controls on every request: the
// hint header (FuzzParseLevel) and, through {client_ip} or a header
// placeholder, the key value (FuzzMaskKey).
package penaltybox

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

// FuzzParseLevel: whatever arrives, the parsed level is always within
// the valid range (garbage degrades to the harmless level 1, never to a
// panic or an out-of-range value).
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

// FuzzMaskKey asserts the properties the budget relies on, for any key
// value s and any 64 host bits:
//   - idempotent: masking a masked key changes nothing;
//   - a value that is not one IP address comes back unchanged;
//   - an IPv4 (or IPv4-mapped) address comes back as that IPv4
//     address, canonically spelled;
//   - an IPv6 address comes back as a canonical /64 prefix holding it;
//   - every other address in that /64 gets the same key, and an address
//     in a different /64 gets a different one.
func FuzzMaskKey(f *testing.F) {
	for _, s := range []string{
		"", "192.0.2.1", "2001:db8:1:2:3:4:5:6", "2001:DB8::1", "::1", "::",
		"::ffff:192.0.2.1", "::ffff:c000:201", "::ffff:192.0.2.1%eth0",
		"fe80::1%eth0", "fe80::1%", "2001:db8::1|example.com", "192.0.2.1:80",
		"[::1]", "2001:db8::/64", "192.000.2.1", " ::1", "\x00", "garbage",
	} {
		f.Add(s, uint64(0))
		f.Add(s, uint64(0x0000ffffc0000201)) // low half of ::ffff:192.0.2.1
	}
	f.Fuzz(func(t *testing.T, s string, low uint64) {
		out := maskKey(s)
		if again := maskKey(out); again != out {
			t.Fatalf("not idempotent: maskKey(%q) = %q, maskKey(that) = %q", s, out, again)
		}

		ip, err := netip.ParseAddr(s)
		if err != nil {
			if out != s {
				t.Fatalf("non-address %q changed to %q", s, out)
			}
			return
		}

		u := ip.Unmap()
		if u.Is4() {
			// Canonical form, so two spellings of one address
			// cannot hold two budgets.
			if out != u.String() {
				t.Fatalf("IPv4 %q: got %q, want %q", s, out, u)
			}
			return
		}

		p, err := netip.ParsePrefix(out)
		if err != nil || p.Bits() != 64 || !p.Addr().Is6() || p != p.Masked() || p.String() != out {
			t.Fatalf("IPv6 %q: got %q, want a canonical /64 prefix", s, out)
		}
		if !p.Contains(u.WithZone("")) {
			t.Fatalf("IPv6 %q: prefix %q does not contain it", s, out)
		}

		// Same /64, other host bits. A sibling that lands on an
		// IPv4-mapped address is an IPv4 client, not a /64 neighbour.
		b := u.As16()
		binary.BigEndian.PutUint64(b[8:], low)
		if sib := netip.AddrFrom16(b); !sib.Is4In6() {
			if got := maskKey(sib.String()); got != out {
				t.Fatalf("same /64: maskKey(%q) = %q, maskKey(%q) = %q", s, out, sib, got)
			}
		}

		// Different /64: flip one of the network bits.
		b = u.As16()
		bit := low % 64
		b[bit/8] ^= 0x80 >> (bit % 8)
		other := netip.AddrFrom16(b)
		if got := maskKey(other.String()); got == out {
			t.Fatalf("different /64: maskKey(%q) and maskKey(%q) both %q", s, other, out)
		}
	})
}
