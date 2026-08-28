//go:build unix

package liveswap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
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

// waitForFile polls until path exists: the worker scripts below touch a
// readiness marker once their trap is installed, so a signal can never
// race the trap (a fixed sleep flaked under -race on a loaded host).
//
// The workers then park in `sleep 30 & wait` rather than looping over
// short foreground sleeps: POSIX guarantees traps run promptly inside
// the wait builtin, whereas a SIGTERM landing while the shell is
// forking its next foreground child was occasionally lost — by both
// dash and bash — which made the trap-based assertions flaky.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitGroupDead polls until no live member of pgid remains, failing the
// test if the group is still populated at the deadline.
func waitGroupDead(t *testing.T, pgid int, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for groupAlive(pgid) {
		if time.Now().After(deadline) {
			_ = signalGroup(pgid, syscall.SIGKILL) // don't leak it past the test
			t.Fatalf("%s: process group %d still alive after %v", what, pgid, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Regression for S2: a leader that exits on its own used to leave its
// workers running forever — callers gate Stop behind Alive, so nothing
// ever signalled the group. The reaper now sweeps it at crash time.
func TestExecRunnerCrashSweepsProcessGroup(t *testing.T) {
	r := testExecRunner()
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild-alive")
	// The grandchild keeps refreshing the marker; the leader exits
	// non-zero almost immediately, orphaning it inside the group.
	script := `(while true; do sleep 0.1; date > ` + marker + `; done) & sleep 0.2; exit 3`
	h, err := r.Start(startSpec{command: []string{"sh", "-c", script}, dir: dir, env: os.Environ(), grace: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pid := h.state().PID
	select {
	case <-r.Wait(h):
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not exit")
	}
	if r.Alive(h) {
		t.Fatal("Alive must report the leader dead")
	}
	// Cooperative grandchild: SIGTERM ends it well inside the grace.
	waitGroupDead(t, pid, time.Second, "crash sweep")
	_ = os.Remove(marker)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("grandchild survived the crash sweep")
	}
	// Stop on the already-exited handle is a harmless no-op.
	must(t, r.Stop(h, time.Second))
}

// The crash sweep escalates like Stop does: a worker that ignores
// SIGTERM is SIGKILLed once the spec's grace runs out.
func TestExecRunnerCrashSweepEscalatesToSIGKILL(t *testing.T) {
	r := testExecRunner()
	script := `(trap "" TERM; while true; do sleep 0.2; done) & sleep 0.2; exit 1`
	h, err := r.Start(startSpec{command: []string{"sh", "-c", script}, dir: t.TempDir(), env: os.Environ(), grace: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	pid := h.state().PID
	<-r.Wait(h)
	start := time.Now()
	waitGroupDead(t, pid, 3*time.Second, "crash sweep escalation")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("survivor outlived the grace by too much: %v", elapsed)
	}
}

// Stop gives the whole group its grace: a leader that dies instantly on
// SIGTERM must not cut short a worker that is still shutting down
// cleanly. The worker's TERM handler leaves a marker; it must exist by
// the time Stop returns, and no SIGKILL may have been needed.
func TestExecRunnerStopWaitsForGroupWithinGrace(t *testing.T) {
	r := testExecRunner()
	dir := t.TempDir()
	clean := filepath.Join(dir, "clean-shutdown")
	ready := filepath.Join(dir, "ready")
	script := `(trap "touch ` + clean + `; exit 0" TERM; touch ` + ready + `; sleep 30 & wait) & wait`
	h, err := r.Start(startSpec{command: []string{"sh", "-c", script}, dir: dir, env: os.Environ(), grace: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pid := h.state().PID
	waitForFile(t, ready) // the trap is installed
	start := time.Now()
	must(t, r.Stop(h, 5*time.Second))
	elapsed := time.Since(start)
	if _, err := os.Stat(clean); err != nil {
		t.Fatalf("Stop returned before the worker finished its clean shutdown: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Stop waited out the grace instead of returning when the group emptied: %v", elapsed)
	}
	waitGroupDead(t, pid, time.Second, "stop")
}

// A worker that ignores SIGTERM after its leader is already gone is
// still swept: Stop spends the remaining grace on the group, then
// SIGKILLs the survivors.
func TestExecRunnerStopKillsGroupSurvivorsAfterGrace(t *testing.T) {
	r := testExecRunner()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	script := `(trap "" TERM; touch ` + ready + `; sleep 30 & wait) & wait`
	h, err := r.Start(startSpec{command: []string{"sh", "-c", script}, dir: dir, env: os.Environ(), grace: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pid := h.state().PID
	waitForFile(t, ready) // TERM is ignored from here on
	start := time.Now()
	must(t, r.Stop(h, 500*time.Millisecond))
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("survivors were killed before the grace period: %v", elapsed)
	}
	waitGroupDead(t, pid, 2*time.Second, "stop survivors")
}

// Once a handle's group has been swept its pgid is dead to us: the
// kernel may have reassigned the number to an unrelated process group,
// and a late Stop (Destruct with the watchdog off, hours later) must
// not signal it. The "unrelated group" here is a live sleep whose pid
// we borrow as the swept handle's pgid.
func TestExecRunnerStopOnSweptHandleNeverSignals(t *testing.T) {
	r := testExecRunner()
	bystander, err := r.Start(startSpec{command: []string{"sleep", "30"}, dir: t.TempDir(), env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Stop(bystander, time.Second) })

	done := make(chan struct{})
	close(done)
	stale := &execHandle{pid: bystander.state().PID, done: done, swept: true}
	must(t, r.Stop(stale, 100*time.Millisecond))
	time.Sleep(100 * time.Millisecond)
	if !r.Alive(bystander) {
		t.Fatal("Stop on a swept handle signalled a recycled pgid")
	}

	// A swept handle replays its sweep's verdict, so a caller that
	// synchronises on Stop before deleting the release learns about
	// leaked workers without anything being signalled.
	wantErr := errors.New("workers leaked")
	stale = &execHandle{pid: bystander.state().PID, done: done, swept: true, sweepErr: wantErr}
	if err := r.Stop(stale, 100*time.Millisecond); !errors.Is(err, wantErr) {
		t.Fatalf("Stop on a swept handle should replay its sweep error, got %v", err)
	}
	if !r.Alive(bystander) {
		t.Fatal("replaying a sweep error must not signal the pgid")
	}

	// Belt and braces: a second Stop on a handle Stop itself swept is
	// also a no-op, so a real sequence (Stop, Destruct) is safe too.
	victim, err := r.Start(startSpec{command: []string{"sleep", "30"}, dir: t.TempDir(), env: os.Environ()})
	must(t, err)
	must(t, r.Stop(victim, time.Second))
	if !victim.(*execHandle).swept {
		t.Fatal("Stop did not record the sweep")
	}
	must(t, r.Stop(victim, time.Second))
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
