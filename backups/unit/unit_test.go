package unit

import (
	"strings"
	"testing"
)

func validSpec() Spec {
	return Spec{Name: "hotserve_backup_test_x.service", Argv: []string{"/bin/true"}, User: "hotserve"}
}

func TestPropertiesRefusesASpecThatIsNotFullySaid(t *testing.T) {
	if _, err := validSpec().properties(); err != nil {
		t.Fatalf("valid spec refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"a name an app's unit could have", func(s *Spec) { s.Name = "hotserve-backup.1.abc.service" }, "does not match"},
		{"a name with a glob in it", func(s *Spec) { s.Name = "hotserve_backup_*.service" }, "does not match"},
		{"a relative command", func(s *Spec) { s.Argv = []string{"restic"} }, "absolute path"},
		{"no command", func(s *Spec) { s.Argv = nil }, "absolute path"},
		{"nobody to run as", func(s *Spec) { s.User = "" }, "exactly one of"},
		{"two answers to who runs it", func(s *Spec) { s.AsRoot = true }, "exactly one of"},
		{"a capability inside a user namespace", func(s *Spec) { s.SameUIDNamespaces = true; s.Capabilities = []Capability{CapDACReadSearch} }, "means nothing inside a user namespace"},
		{"a relative bind", func(s *Spec) { s.Binds = []Bind{{Source: "data"}} }, "absolute"},
		{"a relative mask", func(s *Spec) { s.Masked = []string{"app.db"} }, "not absolute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(&s)
			if _, err := s.properties(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// The properties that must never be absent or spelled the expanding
// way, whatever else a Spec says.
func TestPropertiesAlwaysSandboxAndNeverExpand(t *testing.T) {
	props, err := validSpec().properties()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range props {
		got[p.Name] = p.Value.String()
	}
	if _, ok := got["ExecStart"]; ok {
		t.Error("the command is sent as ExecStart, which the manager expands from the unit's environment")
	}
	if !strings.Contains(got["ExecStartEx"], "no-env-expand") {
		t.Errorf("ExecStartEx = %s, want the no-env-expand flag", got["ExecStartEx"])
	}
	for name, want := range map[string]string{
		"TemporaryFileSystem":   `[("/", "ro",)]`,
		"PrivateNetwork":        "true",
		"NoNewPrivileges":       "true",
		"CapabilityBoundingSet": "@t 0",
		"AmbientCapabilities":   "@t 0",
		"Type":                  `"oneshot"`,
	} {
		if got[name] != want {
			t.Errorf("%s = %s, want %s", name, got[name], want)
		}
	}
	for _, never := range []string{"SuccessExitStatus", "ExecStopPost", "After"} {
		if _, ok := got[never]; ok {
			t.Errorf("%s is set", never)
		}
	}
}
