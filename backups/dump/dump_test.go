package dump

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// inspect needs no sqlite3, so what it turns away is pinned here, in
// the lane that runs everywhere.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestInspect(t *testing.T) {
	shared := t.TempDir()
	outside := t.TempDir()
	db := append([]byte(header), make([]byte, 4080)...)
	must(t, os.WriteFile(filepath.Join(shared, "real.db"), db, 0o644))
	must(t, os.WriteFile(filepath.Join(outside, "outside.db"), db, 0o644))
	must(t, syscall.Mkfifo(filepath.Join(shared, "fifo.db"), 0o644))
	must(t, os.WriteFile(filepath.Join(shared, "empty.db"), nil, 0o644))
	must(t, os.WriteFile(filepath.Join(shared, "short.db"), []byte("SQLite"), 0o644))
	must(t, os.WriteFile(filepath.Join(shared, "text.db"), []byte("not a database, but longer than a header"), 0o644))
	must(t, os.Mkdir(filepath.Join(shared, "dir.db"), 0o755))
	must(t, os.Symlink("real.db", filepath.Join(shared, "link.db")))
	must(t, os.Symlink(filepath.Join(outside, "outside.db"), filepath.Join(shared, "out-link.db")))
	must(t, os.Mkdir(filepath.Join(shared, "sub"), 0o755))
	must(t, os.Symlink(outside, filepath.Join(shared, "sub", "escape")))
	must(t, os.Symlink(".", filepath.Join(shared, "sub", "inside")))

	for rel, want := range map[string]Class{
		"real.db":               OK,
		"sub/inside/../real.db": OK, // a directory link that stays inside is the app's own business
		"fifo.db":               NotADatabase,
		"empty.db":              NotADatabase,
		"short.db":              NotADatabase,
		"text.db":               NotADatabase,
		"dir.db":                NotADatabase,
		"link.db":               NotADatabase,
		"out-link.db":           NotADatabase,
		"absent.db":             Missing,
		"sub/escape/outside.db": Failed, // a way out of the shared dir is refused, not followed
	} {
		done := make(chan Result, 1)
		go func() { done <- inspect(shared, rel) }()
		select {
		case res := <-done:
			if res.Class != want {
				t.Errorf("%s: %+v, want %s", rel, res, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: inspecting it blocked", rel)
		}
	}
}

func TestURI(t *testing.T) {
	for abs, want := range map[string]string{
		"/var/lib/liveswap/blog/shared/app.db": "file:/var/lib/liveswap/blog/shared/app.db?mode=rw",
		"/srv/my dir%20#?/a'b.db":              "file:/srv/my%20dir%2520%23%3F/a%27b.db?mode=rw",
		"/srv/é.db":                            "file:/srv/%C3%A9.db?mode=rw",
	} {
		if got := uri(abs); got != want {
			t.Errorf("uri(%q) = %q, want %q", abs, got, want)
		}
	}
}

// A locked database is waited for before the target exists, so a bound
// on the target's appearing that was not longer than that wait would
// kill an honest sqlite3.
func TestOpenBoundExceedsTheBusyTimeout(t *testing.T) {
	if openWithin < 2*busyTimeout {
		t.Fatalf("openWithin is %s and busyTimeout %s", openWithin, busyTimeout)
	}
}

// The watcher in run, against a real process standing in for sqlite3:
// one that makes its target promptly and then takes far longer than
// the bound, and one that never makes it.
func TestRunBoundsTheOpenAndNotTheCopy(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sqlite3")
	// The target's path arrives in $TARGET; the real arguments are ignored.
	must(t, os.WriteFile(script, []byte("#!/bin/sh\n[ -n \"$SLOW_COPY\" ] && { sleep 0.2; : > \"$TARGET\"; sleep 3; exit 0; }\nexec sleep 600\n"), 0o755))
	oldBin, oldWithin, oldTweak := sqlite3, openWithin, tweakCmd
	sqlite3, openWithin = script, time.Second
	defer func() { sqlite3, openWithin, tweakCmd = oldBin, oldWithin, oldTweak }()

	target := filepath.Join(dir, "copy.db")
	tweakCmd = func(c *exec.Cmd) { c.Env = append(c.Env, "TARGET="+target, "SLOW_COPY=1") }
	start := time.Now()
	if _, _, err := run(context.Background(), "/ignored.db", "ignored", target); err != nil {
		t.Fatalf("a copy that outlasts the bound three times over: %v", err)
	}
	if took := time.Since(start); took < 3*time.Second {
		t.Fatalf("returned after %s, before the process finished", took)
	}

	must(t, os.Remove(target))
	tweakCmd = func(c *exec.Cmd) { c.Env = append(c.Env, "TARGET="+target) }
	start = time.Now()
	_, _, err := run(context.Background(), "/ignored.db", "ignored", target)
	if !errors.Is(err, errNeverOpened) {
		t.Fatalf("a process that never makes its target: %v", err)
	}
	if took := time.Since(start); took < time.Second || took > 10*time.Second {
		t.Fatalf("killed after %s with a 1s bound", took)
	}
}
