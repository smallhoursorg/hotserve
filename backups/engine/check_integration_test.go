//go:build integration

package engine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// resticIn runs the real restic against a local repository under base:
// a measuring stick only, the product has none. It returns stdout,
// stderr and the exit status; a restic that could not be run at all, or
// that outlives within, fails the test.
func resticIn(t *testing.T, base string, within time.Duration, argv ...string) (stdout, stderr string, exit int) {
	t.Helper()
	if _, err := os.Stat(argv[0]); err != nil {
		t.Fatalf("%s is not installed in the integration image: %v", argv[0], err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "RESTIC_PASSWORD=pw", "RESTIC_REPOSITORY="+filepath.Join(base, "repo"), "RESTIC_CACHE_DIR="+filepath.Join(base, "cache"))
	cmd.Dir = base
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("restic %q did not end within %s: %s", argv[1:], within, errOut.String())
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("restic %q: %v", argv[1:], err)
	}
	return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
}

// The check reads one fifty-second of the data a week, and a pack
// rotten inside — its size unchanged — is found by the week whose group
// holds it and by no other, nor by the structure alone [M74]: the group
// is the pack id's first byte mod 52, plus one. That is what makes the
// fifty-two weeks read every pack once. And a repository restic could
// not open at all gets the same summary as damage, with an error in it
// [M73]: the verdict never reads the summary alone.
func TestIntegrationTheCheckFindsARottenPackOnlyInItsGroup(t *testing.T) {
	const restic = "/usr/bin/restic"
	base := t.TempDir()
	noise := make([]byte, 4<<20)
	_, _ = rand.Read(noise)
	must(t, os.WriteFile(filepath.Join(base, "f"), noise, 0o644))
	resticIn(t, base, time.Minute, restic, "init", "-q")
	if _, said, exit := resticIn(t, base, time.Minute, restic, "backup", "-q", "f"); exit != 0 {
		t.Fatalf("fixture: backup exit %d: %s", exit, said)
	}
	packs, err := filepath.Glob(filepath.Join(base, "repo", "data", "*", "*"))
	must(t, err)
	var pack string
	for _, p := range packs {
		if st, err := os.Stat(p); err == nil && st.Size() > 2<<20 {
			pack = p
		}
	}
	if pack == "" {
		t.Fatalf("fixture: no data pack over 2 MiB among %v", packs)
	}
	id := filepath.Base(pack)
	first, err := strconv.ParseUint(id[:2], 16, 8)
	must(t, err)
	group := int(first)%checkGroups + 1
	must(t, os.Chmod(pack, 0o600))
	f, err := os.OpenFile(pack, os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteAt([]byte("sixteen bytes!!!"), 1<<20)
	must(t, errors.Join(err, f.Close()))

	summary := func(out string) (errs int, broken []string) {
		var s struct {
			NumErrors   int      `json:"num_errors"`
			BrokenPacks []string `json:"broken_packs"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &s); err != nil {
			t.Fatalf("the summary %q: %v", out, err)
		}
		return s.NumErrors, s.BrokenPacks
	}
	// The group that holds it.
	out, _, exit := resticIn(t, base, time.Minute, checkArgv(restic, fmt.Sprintf("%d/%d", group, checkGroups))...)
	if errs, broken := summary(out); exit != 1 || errs == 0 || !slices.Contains(broken, id) {
		t.Errorf("group %d, which holds pack %.8s: exit %d, %d errors, broken %v", group, id, exit, errs, broken)
	}
	// Another group, and the structure alone.
	other := group%checkGroups + 1
	if out, said, exit := resticIn(t, base, time.Minute, checkArgv(restic, fmt.Sprintf("%d/%d", other, checkGroups))...); exit != 0 {
		t.Errorf("group %d, which does not hold it: exit %d: %s %s", other, exit, out, said)
	}
	if out, said, exit := resticIn(t, base, time.Minute, restic, "check", "--no-lock", "--json"); exit != 0 {
		t.Errorf("the structure alone: exit %d: %s %s", exit, out, said)
	}
	// Where there is no repository: exit 10, and a summary with an error.
	must(t, os.Rename(filepath.Join(base, "repo"), filepath.Join(base, "gone")))
	out, _, exit = resticIn(t, base, time.Minute, checkArgv(restic, "1/52")...)
	if errs, _ := summary(out); exit != 10 || errs == 0 {
		t.Errorf("no repository: exit %d, %d errors in %q", exit, errs, out)
	}
}

// The check writes no lock [M75; the owner, 2026-09-29]: a check that
// dies hard with a lock written leaves it, and restic never passes a
// stale lock by itself, so every backup after would exit 11 until
// someone ran `restic unlock`. With the lock directory made a file, a
// check that writes a lock can never write it and retries; one that
// writes none ends at once.
func TestIntegrationTheCheckWritesNoLock(t *testing.T) {
	const restic = "/usr/bin/restic"
	base := t.TempDir()
	must(t, os.WriteFile(filepath.Join(base, "f"), []byte("data"), 0o644))
	resticIn(t, base, time.Minute, restic, "init", "-q")
	resticIn(t, base, time.Minute, restic, "backup", "-q", "f")
	locks := filepath.Join(base, "repo", "locks")
	must(t, os.Remove(locks))
	must(t, os.WriteFile(locks, []byte("not a directory: a lock written here fails"), 0o600))
	if out, said, exit := resticIn(t, base, 20*time.Second, checkArgv(restic, "1/52")...); exit != 0 {
		t.Errorf("the check: exit %d: %s %s", exit, out, said)
	}
	if _, said, exit := resticIn(t, base, 20*time.Second, restic, "cat", "config", "--no-lock"); exit != 0 {
		t.Errorf("the probe: exit %d: %s", exit, said)
	}
}

// A clean-run record is a snapshot of its own [M76]: of the id it
// vouches for, which it holds — an empty one is restic's exit 3 — in a
// group of its own, and never among the app's snapshots.
func TestIntegrationACleanRunRecordIsASnapshotOfItsOwn(t *testing.T) {
	const restic = "/usr/bin/restic"
	base := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(base, "backup", "blog"), 0o755))
	must(t, os.WriteFile(filepath.Join(base, "backup", "blog", "f"), []byte("data"), 0o644))
	resticIn(t, base, time.Minute, restic, "init", "-q")
	out, said, exit := resticIn(t, base, time.Minute, restic, "backup", "--quiet", "--json", "--host", "hotserve", "--tag", "hotserve", "--tag", "app:blog", "backup/blog")
	must(t, os.WriteFile(filepath.Join(base, "summary"), []byte(out), 0o600))
	id := summaryID(filepath.Join(base, "summary"))
	if exit != 0 || id == "" {
		t.Fatalf("fixture: exit %d, %q %s", exit, out, said)
	}
	// At the snapshot's own time, to the second, read in the zone the
	// unit is given.
	made := time.Date(2026, 9, 2, 7, 8, 9, 123456789, time.UTC)
	argv := vouchArgv(restic, "blog", id, made)
	t.Setenv("TZ", "UTC")
	out, said, exit = resticIn(t, base, time.Minute, argv...)
	must(t, os.WriteFile(filepath.Join(base, "record"), []byte(out), 0o600))
	record := summaryID(filepath.Join(base, "record"))
	if exit != 0 || record == "" || said != "" {
		t.Fatalf("the record: exit %d, %q, stderr %q", exit, out, said)
	}
	var snaps []struct {
		ID    string    `json:"id"`
		Time  time.Time `json:"time"`
		Paths []string  `json:"paths"`
		Tags  []string  `json:"tags"`
	}
	list := func(tag string) {
		t.Helper()
		out, said, exit := resticIn(t, base, time.Minute, restic, "snapshots", "--json", "--no-lock", "--host", "hotserve", "--tag", tag)
		snaps = nil
		if exit != 0 || json.Unmarshal([]byte(out), &snaps) != nil {
			t.Fatalf("snapshots --tag %s: exit %d, %q %s", tag, exit, out, said)
		}
	}
	list("app:blog")
	if len(snaps) != 1 || snaps[0].ID != id {
		t.Errorf("the app's snapshots hold the record: %+v", snaps)
	}
	list("hotserve-clean")
	if len(snaps) != 1 || snaps[0].ID != record || !slices.Equal(snaps[0].Paths, []string{"/hotserve-clean-blog"}) || !slices.Contains(snaps[0].Tags, "vouches:"+id) {
		t.Errorf("the record: %+v", snaps)
	}
	if len(snaps) == 1 && !snaps[0].Time.Equal(made.Truncate(time.Second)) {
		t.Errorf("the record's time is %s, not the snapshot's %s to the second", snaps[0].Time, made)
	}
	if held, _, exit := resticIn(t, base, time.Minute, restic, "dump", "--no-lock", record, "/hotserve-clean-blog"); exit != 0 || strings.TrimSpace(held) != id {
		t.Errorf("the record holds %q (exit %d), not the id it vouches for", held, exit)
	}
	// Why it holds the id: a record of nothing is restic's exit 3.
	empty := slices.Clone(argv)
	empty[len(empty)-2], empty = "/bin/true", empty[:len(empty)-1]
	if _, _, exit := resticIn(t, base, time.Minute, empty...); exit != 3 {
		t.Errorf("a record of nothing: exit %d, want restic's 3", exit)
	}
}

// restic stores a snapshot's time in the zone it runs in, and forget
// sorts each snapshot into its days by that zone [measured]: so every
// restic unit is given UTC (resticUnit), whatever the box's own zone —
// here Berlin's — and a snapshot and its record, both in UTC, fall into
// the same days.
func TestIntegrationAResticUnitStoresItsTimesInUTC(t *testing.T) {
	const restic = "/usr/bin/restic"
	base := t.TempDir()
	must(t, os.WriteFile(filepath.Join(base, "f"), []byte("data"), 0o644))
	t.Setenv("TZ", "Europe/Berlin")
	resticIn(t, base, time.Minute, restic, "init", "-q")
	stored := func() string {
		t.Helper()
		out, said, exit := resticIn(t, base, time.Minute, restic, "snapshots", "--json", "--no-lock", "latest")
		var snaps []struct {
			Time string `json:"time"`
		}
		if exit != 0 || json.Unmarshal([]byte(out), &snaps) != nil || len(snaps) != 1 {
			t.Fatalf("snapshots: exit %d, %q %s", exit, out, said)
		}
		return snaps[0].Time
	}
	// The box's zone, as a unit sees it without being told otherwise.
	resticIn(t, base, time.Minute, restic, "backup", "-q", "f")
	if got := stored(); !strings.HasSuffix(got, "+02:00") && !strings.HasSuffix(got, "+01:00") {
		t.Fatalf("fixture: restic in Berlin stored %s: the zone it runs in is not what it stores", got)
	}
	// The environment a restic unit is given.
	for _, kv := range resticUnit("hotserve_backup_x_000000000000.service", "", "", []string{restic}, "").Environment {
		k, v, _ := strings.Cut(kv, "=")
		if k == "TZ" {
			t.Setenv(k, v)
		}
	}
	resticIn(t, base, time.Minute, restic, "backup", "-q", "f")
	if got := stored(); !strings.HasSuffix(got, "Z") {
		t.Errorf("a restic unit stored %s, not UTC", got)
	}
}
