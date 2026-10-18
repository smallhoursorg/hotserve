package liveswap

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseClaimsFlagRejectsReserved(t *testing.T) {
	for _, s := range []string{"aud=other", "sub=x", "exp=0", "iat=0", "jti=x", "iss=y"} {
		if _, err := parseClaimsFlag(s); err == nil {
			t.Errorf("reserved claim %q must be rejected", s)
		}
	}
	m, err := parseClaimsFlag("repository=o/r,ref=refs/heads/main")
	if err != nil {
		t.Fatalf("valid claims rejected: %v", err)
	}
	if m["repository"] != "o/r" || m["ref"] != "refs/heads/main" {
		t.Fatalf("claims parsed wrong: %v", m)
	}
}

// A --ttl over an hour is minted with a warning on stderr saying what
// a box does with it; an hour or less is minted silently.
func TestDeployTokenWarnsOverAnHour(t *testing.T) {
	for ttl, warns := range map[time.Duration]bool{
		5 * time.Minute:         false,
		time.Hour:               false,
		time.Hour + time.Second: true,
		87600 * time.Hour:       true,
	} {
		got := ttlWarning(ttl)
		if (got != "") != warns {
			t.Errorf("--ttl %v: warning %q, want one: %t", ttl, got, warns)
		}
		if warns && (!strings.Contains(got, ttl.String()) || !strings.Contains(got, "max_token_ttl")) {
			t.Errorf("--ttl %v: warning %q, want it to name the ttl and max_token_ttl", ttl, got)
		}
	}
}

// The lifetime a token encodes, exp - iat in whole seconds, is --ttl
// rounded down, never up, however fractional the mint time: so a
// token minted with --ttl equal to a box's fractional max_token_ttl
// verifies there, and a whole-second --ttl keeps its whole lifetime.
func TestDeployTokenLifetimeRoundsDown(t *testing.T) {
	at := time.Unix(1_700_000_000, 750_000_000) // a mint at .750 s
	for ttl, want := range map[time.Duration]int64{
		1500 * time.Millisecond: 1,
		1400 * time.Millisecond: 1,
		time.Second:             1,
		5 * time.Minute:         300,
	} {
		iat, exp := tokenTimes(at, ttl)
		if got := int64(*exp) - int64(*iat); got != want {
			t.Errorf("--ttl %v minted at .750 s encodes %d s, want %d", ttl, got, want)
		}
	}
}

func TestWriteNewFileRefusesOverwrite(t *testing.T) {
	path := t.TempDir() + "/deploy.key"
	if err := writeNewFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("first write failed: %v", err)
	}
	if err := writeNewFile(path, []byte("second"), 0o600); err == nil {
		t.Fatal("overwriting an existing key must be refused")
	}
	// The original content and mode are intact.
	data, _ := os.ReadFile(path)
	if string(data) != "first" {
		t.Fatalf("existing file was modified: %q", data)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
