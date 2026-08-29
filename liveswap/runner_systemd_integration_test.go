//go:build integration

package liveswap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// These run against a real systemd user manager (make test-integration
// boots one in the dev-systemd container and points XDG_RUNTIME_DIR at
// it). They prove the properties unit tests can only assume: that
// "stop" and "crash" empty the whole cgroup, that the manager reports
// exits the way the runner reads them, and that a second runner can
// adopt a unit the first one created.

func integrationRunner(t *testing.T) *systemdRunner {
	t.Helper()
	if err := probeUserManager(); err != nil {
		t.Fatalf("no systemd user manager (run via `make test-integration`): %v", err)
	}
	logger, _ := zap.NewDevelopment()
	r := newSystemdRunner(userManager, logger.Named(t.Name()))
	r.poll = 50 * time.Millisecond
	t.Cleanup(r.close)
	return r
}

// scriptApp writes ./server with the given body into a release dir.
func scriptApp(t *testing.T, body string) startSpec {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return startSpec{
		app:     "itest",
		version: strings.ToLower(strings.TrimPrefix(t.Name(), "TestIntegrationSystemd")),
		command: []string{"./server"},
		dir:     dir,
		env:     []string{"PORT=4321", "HOST=127.0.0.1", "PATH=" + os.Getenv("PATH")},
		grace:   2 * time.Second,
	}
}

// workerTree is a leader that forks two workers and records all three
// PIDs in pids.txt, the shape (npm → node → …) cgroup kill exists for.
const workerTree = `sleep 300 & w1=$!
sleep 300 & w2=$!
echo "$$ $w1 $w2" > pids.txt
wait
`

func readPIDs(t *testing.T, dir string) []int {
	t.Helper()
	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(dir, "pids.txt"))
		if err == nil && strings.Count(string(b), " ") == 2 {
			data = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pids.txt never written: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var pids []int
	for _, f := range strings.Fields(string(data)) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, n)
	}
	return pids
}

func alivePID(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	// EPERM etc.: exists but not ours — still alive.
	// Zombies also answer kill(0); systemd reaps its units' children,
	// so a lingering zombie here would itself be a bug.
	return true
}

func waitPIDsGone(t *testing.T, pids []int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var live []int
		for _, p := range pids {
			if alivePID(p) {
				live = append(live, p)
			}
		}
		if len(live) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still alive after %s: %v", within, live)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestIntegrationSystemdStopKillsWholeTree(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, workerTree)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	pids := readPIDs(t, spec.dir)
	if h.state().PID != pids[0] {
		t.Fatalf("handle PID %d, leader wrote %d", h.state().PID, pids[0])
	}
	if !r.Alive(h) {
		t.Fatal("instance should be alive")
	}
	if err := r.Stop(h, spec.grace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop returned nil ⇒ the cgroup was empty (invariant 4); the
	// workers must be gone already, not "soon".
	waitPIDsGone(t, pids, 200*time.Millisecond)
	if r.Alive(h) {
		t.Fatal("stopped instance reads alive")
	}
	if st, err := userManager.UnitStatus(context.Background(), h.state().Unit); err != nil || st.loaded() {
		t.Fatalf("stopped unit must be unloaded, got %+v (%v)", st, err)
	}
}

func TestIntegrationSystemdLeaderCrashKillsWorkers(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, `sleep 300 & w1=$!
sleep 300 & w2=$!
echo "$$ $w1 $w2" > pids.txt
sleep 0.3
exit 3
`)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	pids := readPIDs(t, spec.dir)
	select {
	case <-r.Wait(h):
	case <-time.After(10 * time.Second):
		t.Fatal("crash never observed")
	}
	// With KillMode=control-group the leader's exit takes the workers
	// with it; done closes only once the manager reports the unit
	// gone, i.e. after they are dead.
	waitPIDsGone(t, pids, 200*time.Millisecond)
	exit := h.(*systemdHandle).exit.Load()
	if exit == nil || exit.exitString() != "exit status 3" {
		t.Fatalf("exit facts %+v", exit)
	}
	if st, err := userManager.UnitStatus(context.Background(), h.state().Unit); err != nil || st.loaded() {
		t.Fatalf("failed unit must have been reset and unloaded, got %+v (%v)", st, err)
	}
}

func TestIntegrationSystemdStopEscalatesToSIGKILL(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, "trap '' TERM\necho \"$$\" > pids.txt\nwhile :; do sleep 1; done\n")
	spec.grace = time.Second
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(spec.dir, "pids.txt")); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	started := time.Now()
	if err := r.Stop(h, spec.grace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	took := time.Since(started)
	if took < spec.grace {
		t.Fatalf("a TERM-ignoring app was killed after %s, before its %s grace", took, spec.grace)
	}
	if took > spec.grace+3*time.Second {
		t.Fatalf("SIGKILL escalation took %s", took)
	}
	if r.Alive(h) {
		t.Fatal("instance still alive after escalation")
	}
}

func TestIntegrationSystemdRunOnce(t *testing.T) {
	r := integrationRunner(t)
	ok := scriptApp(t, "echo \"$PORT $(pwd)\" > out.txt\nexit 0\n")
	if err := r.RunOnce(context.Background(), ok); err != nil {
		t.Fatalf("RunOnce ok: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(ok.dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(out))
	realDir, _ := filepath.EvalSymlinks(ok.dir)
	if len(fields) != 2 || fields[0] != "4321" || fields[1] != realDir {
		t.Fatalf("env/cwd not propagated: %q (want 4321 %s)", out, realDir)
	}

	bad := scriptApp(t, "exit 4\n")
	if err := r.RunOnce(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "exit status 4") {
		t.Fatalf("non-zero exit: %v", err)
	}

	slow := scriptApp(t, "echo \"$$\" > pids.txt\nsleep 300\n")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err = r.RunOnce(ctx, slow)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled RunOnce: %v", err)
	}
	b, readErr := os.ReadFile(filepath.Join(slow.dir, "pids.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	waitPIDsGone(t, []int{pid}, 5*time.Second)
}

func TestIntegrationSystemdReattachAdoptsLiveUnit(t *testing.T) {
	r1 := integrationRunner(t)
	spec := scriptApp(t, workerTree)
	h1, err := r1.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	pids := readPIDs(t, spec.dir)
	st := h1.state()
	if st.Unit == "" {
		t.Fatal("state must name the unit")
	}
	// A "new hotserve": its own runner, same manager.
	r2 := newSystemdRunner(userManager, zap.NewNop())
	r2.poll = 50 * time.Millisecond
	t.Cleanup(r2.close)
	h2, ok := r2.Reattach(st)
	if !ok {
		t.Fatal("live unit must be adopted")
	}
	if h2.state().PID != pids[0] || h2.state().Unit != st.Unit {
		t.Fatalf("adopted %+v, want pid %d unit %s", h2.state(), pids[0], st.Unit)
	}
	if err := r2.Stop(h2, spec.grace); err != nil {
		t.Fatalf("Stop via adopter: %v", err)
	}
	waitPIDsGone(t, pids, 200*time.Millisecond)
	// The original runner's watcher sees the same truth.
	select {
	case <-r1.Wait(h1):
	case <-time.After(5 * time.Second):
		t.Fatal("original handle never saw the unit go")
	}
	if _, ok := r2.Reattach(st); ok {
		t.Fatal("a stopped unit must not be adoptable")
	}
}

func TestIntegrationSystemdReattachResetsFailedUnit(t *testing.T) {
	r1 := integrationRunner(t)
	spec := scriptApp(t, "exit 7\n")
	h, err := r1.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	// Silence r1's watcher before it can reset the failure, standing in
	// for "hotserve was down when the app died".
	r1.close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := userManager.UnitStatus(context.Background(), h.state().Unit)
		if err == nil && st.ActiveState == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit never reached failed: %+v (%v)", st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	r2 := newSystemdRunner(userManager, zap.NewNop())
	t.Cleanup(r2.close)
	if _, ok := r2.Reattach(h.state()); ok {
		t.Fatal("a failed unit must never be adopted")
	}
	st, err := userManager.UnitStatus(context.Background(), h.state().Unit)
	if err != nil || st.loaded() {
		t.Fatalf("failed unit must be reset on discovery, got %+v (%v)", st, err)
	}
}
