// Fuzz targets for the surfaces the systemd runner added: unit names
// derived from app names and version tags (a version tag comes off the
// deploy webhook), env keys from env_file (systemd rejects what execve
// accepted), and the values read back from the service manager over
// D-Bus. As in fuzz_test.go, properties are asserted rather than mere
// absence of panics: a unit name must round-trip to exactly its app
// and never match a sibling (the "blog" vs "blog-api" class of bug),
// every env key handed to a unit must be one systemd accepts, and no
// manager value may become a bogus stop budget or limit.
package liveswap

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// systemdUnitNameRe is what systemd itself accepts for a unit name
// (unit_name_is_valid, plain names): a restricted alphabet, no "@"
// (that would make it a template instance), at most 255 bytes.
var systemdUnitNameRe = regexp.MustCompile(`^[A-Za-z0-9:._\\-]{1,255}$`)

func FuzzUnitName(f *testing.F) {
	for _, c := range [][2]string{
		{"blog", "v1.4.2"}, {"blog-api", "v1"}, {"blog", "1"}, {"a", "."}, {"a", ".."},
		{"Blog", "v1"}, {"blog", "v1/../x"}, {"", "v1"}, {"blog", ""}, {"blog@x", "v1"},
		{strings.Repeat("a", 63), strings.Repeat("9", 64)}, {strings.Repeat("a", 64), "v1"},
		{"b-", "-"}, {"-", "v-1"}, {"blog", "v1.prestart"}, {"blog", "1.aaaaaaaa"},
	} {
		f.Add(c[0], c[1])
	}
	f.Fuzz(func(t *testing.T, app, version string) {
		spec := startSpec{app: app, version: version}
		valid := appNameRe.MatchString(app) && validVersion(version)
		for _, oneshot := range []bool{false, true} {
			name, err := unitName(spec, oneshot)
			if !valid {
				if err == nil {
					t.Fatalf("unitName(%q, %q) = %q for an invalid app/version", app, version, name)
				}
				continue
			}
			if err != nil {
				t.Fatalf("unitName(%q, %q): %v", app, version, err)
			}
			if !systemdUnitNameRe.MatchString(name) || strings.Contains(name, "@") {
				t.Fatalf("%q is not a valid systemd unit name", name)
			}
			if got, ok := unitApp(name); !ok || got != app {
				t.Fatalf("unitApp(%q) = %q,%v; want %q", name, got, ok, app)
			}
			if !unitBelongsTo(name, app) {
				t.Fatalf("unitBelongsTo(%q, %q) = false", name, app)
			}
			if oneshot != strings.HasSuffix(name, ".prestart.service") {
				t.Fatalf("prestart suffix mismatch for %q (oneshot=%v)", name, oneshot)
			}
			// No sibling app may claim this unit: not one whose name is
			// a prefix of ours, not one that extends ours.
			for _, other := range []string{app + "-x", strings.TrimRight(app, "-"), app[:len(app)/2]} {
				if other == app || !appNameRe.MatchString(other) {
					continue
				}
				if unitBelongsTo(name, other) {
					t.Fatalf("unitBelongsTo(%q, %q) = true: unit of %q claimed by a sibling", name, other, app)
				}
			}
		}
	})
}

func FuzzParseEnvFile(f *testing.F) {
	for _, s := range []string{
		"GOOD=1\n", "export A=b\n", "# c\n\nK=\"v\"\n", "my-var=1\n", "1X=2\n", "=v\n", "K\n",
		"K=v=w\n", "K='a b'\n", "K=\x00\n", "\xff\xfe\n", "K=" + strings.Repeat("x", 1<<16) + "\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "app.env")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		vars, err := parseEnvFile(path)
		if err != nil {
			if !strings.Contains(err.Error(), path+":") {
				t.Fatalf("error must name the file and line: %v", err)
			}
			return
		}
		for _, kv := range vars {
			key, _, found := strings.Cut(kv, "=")
			if !found || !validEnvKey(key) {
				t.Fatalf("parseEnvFile returned %q, which systemd would reject", kv)
			}
		}
	})
}

func FuzzParseManagerUint(f *testing.F) {
	for _, s := range []string{"@t 524288", "524288", "@t 18446744073709551615", "-1", "", "@t", "0x10", " @t 7 "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n := parseManagerUint(s)
		if n == ^uint64(0) {
			t.Fatalf("parseManagerUint(%q) returned the unlimited sentinel; that must read as unknown", s)
		}
		if n != 0 {
			if got := parseManagerUint("@t " + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@t"))); got != n {
				t.Fatalf("canonical form of %q parses to %d, not %d", s, got, n)
			}
		}
	})
}

func FuzzUsecDuration(f *testing.F) {
	for _, v := range []int{0, 1, 60_000_000, -1, int(^uint64(0) >> 1), int(time.Duration(1<<62) / time.Microsecond)} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, usec int) {
		d := usecDuration(usec)
		if d < 0 {
			t.Fatalf("usecDuration(%d) = %s: negative", usec, d)
		}
		if d > 0 && int(d/time.Microsecond) != usec {
			t.Fatalf("usecDuration(%d) = %s: does not round-trip", usec, d)
		}
	})
}

func FuzzPropInt(f *testing.F) {
	f.Add(int64(3), uint64(77), "x")
	f.Add(int64(-1), ^uint64(0), "")
	f.Fuzz(func(t *testing.T, i int64, u uint64, s string) {
		props := map[string]any{"a": i, "b": u, "c": s, "d": int32(i), "e": uint32(u)}
		for k := range props {
			_ = propInt(props, k) // any narrowing is fine; panics are not
		}
		if propInt(props, "missing") != 0 || propInt(props, "c") != 0 {
			t.Fatal("absent or non-numeric properties read as 0")
		}
		st := statusFromProps(map[string]any{"MainPID": u, "ExecMainCode": i, "TimeoutStopUSec": u})
		if st.StopTimeout < 0 {
			t.Fatalf("negative stop budget from %d", u)
		}
		_ = st.exitString()
	})
}
