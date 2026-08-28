//go:build unix

package liveswap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func testExecRunner() *execRunner { return newExecRunner(zap.NewNop()) }

func TestExecRunnerStartStop(t *testing.T) {
	r := testExecRunner()
	h, err := r.Start(startSpec{command: []string{"sleep", "30"}, dir: t.TempDir(), env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Alive(h) {
		t.Fatal("process should be alive")
	}
	if err := r.Stop(h, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if r.Alive(h) {
		t.Fatal("process should be dead after Stop")
	}
}

func TestExecRunnerStopKillsProcessGroup(t *testing.T) {
	r := testExecRunner()
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild-alive")
	// The shell spawns a grandchild that would keep writing the marker
	// if it survived the group kill. sh ignores nothing, so SIGTERM to
	// the group takes both out.
	script := `(while true; do sleep 0.1; date > ` + marker + `; done) & wait`
	h, err := r.Start(startSpec{command: []string{"sh", "-c", script}, dir: dir, env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the grandchild start writing
	must(t, r.Stop(h, 2*time.Second))
	// If the grandchild survived, the marker keeps getting refreshed.
	_ = os.Remove(marker)
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("grandchild survived the process-group stop")
	}
}

// TestExecRunnerReaperSweepsGroupOnCrash covers the crash path: the
// leader exits on its own (no Stop), leaving a child it spawned into its
// process group. Without the reaper's group sweep that child is orphaned
// and keeps refreshing the marker forever.
func TestExecRunnerReaperSweepsGroupOnCrash(t *testing.T) {
	r := testExecRunner()
	dir := t.TempDir()
	marker := filepath.Join(dir, "orphan-alive")
	// Spawn a grandchild into the group, then let the leader exit by
	// itself — no Stop is ever called.
	script := `(while true; do sleep 0.1; date > ` + marker + `; done) & sleep 0.3; exit 0`
	h, err := r.Start(startSpec{command: []string{"sh", "-c", script}, dir: dir, env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Wait(h) // leader exits on its own; the reaper sweeps before closing done
	if r.Alive(h) {
		t.Fatal("leader should have exited")
	}
	// If the orphan survived it would recreate the marker after this.
	_ = os.Remove(marker)
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("orphaned grandchild survived the leader's crash")
	}
}

func TestExecRunnerStopEscalatesToSIGKILL(t *testing.T) {
	r := testExecRunner()
	// Trap and ignore TERM; only SIGKILL can end this.
	h, err := r.Start(startSpec{
		command: []string{"sh", "-c", `trap "" TERM; while true; do sleep 0.2; done`},
		dir:     t.TempDir(), env: os.Environ(),
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the trap install
	start := time.Now()
	must(t, r.Stop(h, 500*time.Millisecond))
	if r.Alive(h) {
		t.Fatal("process survived SIGKILL escalation")
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("SIGKILL fired before the grace period: %v", elapsed)
	}
}

func TestExecRunnerRunOnceSuccessAndFailure(t *testing.T) {
	r := testExecRunner()
	ctx := context.Background()
	if err := r.RunOnce(ctx, startSpec{command: []string{"true"}, dir: t.TempDir(), env: os.Environ()}); err != nil {
		t.Fatalf("true should succeed: %v", err)
	}
	if err := r.RunOnce(ctx, startSpec{command: []string{"false"}, dir: t.TempDir(), env: os.Environ()}); err == nil {
		t.Fatal("false should fail")
	}
}

func TestExecRunnerRunOnceHonorsContext(t *testing.T) {
	r := testExecRunner()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.RunOnce(ctx, startSpec{command: []string{"sleep", "30"}, dir: t.TempDir(), env: os.Environ()})
	if err == nil {
		t.Fatal("expected context error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("RunOnce did not kill the process on cancellation")
	}
}

func TestExecRunnerWorkingDirAndEnv(t *testing.T) {
	r := testExecRunner()
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	err := r.RunOnce(context.Background(), startSpec{
		command: []string{"sh", "-c", "pwd > out; echo $PORT >> out"},
		dir:     dir,
		env:     append(os.Environ(), "PORT=7777"),
	})
	must(t, err)
	data, err := os.ReadFile(out)
	must(t, err)
	want := dir + "\n7777\n"
	if got := string(data); got != want {
		// macOS TempDir has a /private prefix ambiguity; compare suffix.
		if !filepath.IsAbs(dir) || !stringsHasSuffix(got, want) {
			t.Fatalf("cwd/env wrong: got %q want %q", got, want)
		}
	}
}

func stringsHasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func TestExecRunnerReattachAlwaysFails(t *testing.T) {
	r := testExecRunner()
	if _, ok := r.Reattach(handleState{PID: os.Getpid()}); ok {
		t.Fatal("exec runner must never claim to reattach")
	}
}

func TestFreePortAllocates(t *testing.T) {
	p1, err := freePort()
	must(t, err)
	if p1 <= 0 || p1 > 65535 {
		t.Fatalf("bogus port %d", p1)
	}
}

// Regression for the log-pipe liveness bug: a single line beyond the
// scanner's 1MB cap used to kill the pipe-draining goroutine, after
// which the child filled the ~64KB OS pipe buffer and blocked forever
// on its next write. The fix logs the scan error and drains to EOF;
// this test wedges permanently without it.
func TestPipeLinesSurvivesOversizedLine(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	r := newExecRunner(zap.New(core))
	// One 2MB line, then ~150KB of ordinary output — more than any OS
	// pipe buffer, so an undrained pipe guarantees a blocked child.
	script := `head -c 2097152 /dev/zero | tr '\0' a; echo; i=0; ` +
		`while [ $i -lt 10000 ]; do echo "filler line $i"; i=$((i+1)); done`
	h, err := r.Start(startSpec{command: []string{"sh", "-c", script}, dir: t.TempDir(), env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for r.Alive(h) {
		if time.Now().After(deadline) {
			_ = r.Stop(h, 0)
			t.Fatal("child still running: log pipe wedged after oversized line")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if logs.FilterMessageSnippet("log pipe scan failed").Len() == 0 {
		t.Fatal("expected a scan-failure warning to be logged")
	}
}
