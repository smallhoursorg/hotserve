//go:build unix

package liveswap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
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
