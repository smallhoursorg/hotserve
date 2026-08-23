package liveswap

import (
	"os"
	"testing"
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
