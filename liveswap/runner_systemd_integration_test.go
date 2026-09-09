//go:build integration

package liveswap

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	if err := userManager.probe(); err != nil {
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
		nonce:   "0a1b2c3d0a1b2c3d",
		command: []string{"./server"},
		dir:     dir,
		env:     []string{"SOCKET=/var/lib/liveswap/itest/run/0a1b2c3d0a1b2c3d.sock", "PATH=" + os.Getenv("PATH")},
		grace:   2 * time.Second,
		sandbox: itestSandbox(dir),
	}
}

// itestSandbox is the sandbox every integration unit runs in: the
// script's own directory bound writable, under a root nothing else
// uses. Every unit is sandboxed, so the lane's are too — which puts
// each in its own PID namespace, where the pids a script writes are
// namespace-local. unitPIDs reads the host pids from the cgroup.
func itestSandbox(dir string) *sandboxSpec {
	return &sandboxSpec{root: "/var/tmp/liveswap-itest", writable: []bindPath{{dest: dir, source: dir}}}
}

// unitPIDs is the unit's process set as the manager's own ledger has
// it — host pids from the unit's cgroup — the same way the e2e lane
// reads them.
func unitPIDs(t *testing.T, unit string) []int {
	t.Helper()
	cg := strings.TrimSpace(run(t, "systemctl", "--user", "show", "-p", "ControlGroup", "--value", unit))
	if cg == "" {
		t.Fatalf("unit %s has no control group", unit)
	}
	b, err := os.ReadFile("/sys/fs/cgroup" + cg + "/cgroup.procs")
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, n)
	}
	return pids
}

func hasPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
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
	readPIDs(t, spec.dir) // readiness: the workers are forked
	pids := unitPIDs(t, h.state().Unit)
	if len(pids) < 3 || !hasPID(pids, h.state().PID) {
		t.Fatalf("cgroup holds %v; want the leader (handle pid %d) and two workers", pids, h.state().PID)
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
sleep 1
exit 3
`)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	readPIDs(t, spec.dir) // readiness: the workers are forked
	pids := unitPIDs(t, h.state().Unit)
	if len(pids) < 3 {
		t.Fatalf("cgroup holds %v; want the leader and two workers", pids)
	}
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

// The unit owns its socket file: ExecStopPost= removes it after the
// service stops, however it stopped, with no supervisor-side cleanup
// — which is what tidies up while hotserve is down. The socket is
// bound from outside at the path the unit sees (its dir is a writable
// bind at the same real path), and neither Stop nor a crash calls
// socketRef.retire here.
func TestIntegrationSystemdUnitRemovesItsSocketWhenItStops(t *testing.T) {
	r := integrationRunner(t)
	for _, tc := range []struct {
		name   string
		script string
		stop   bool
	}{
		{"stopped", "while :; do sleep 1; done\n", true},
		{"crashed", "sleep 1\nexit 3\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := scriptApp(t, tc.script)
			spec.version = "socket-" + tc.name // scriptApp's t.Name()-derived version carries the subtest's "/"
			spec.socket = filepath.Join(spec.dir, "app.sock")
			ln, err := net.Listen("unix", spec.socket)
			if err != nil {
				t.Fatal(err)
			}
			ln.(*net.UnixListener).SetUnlinkOnClose(false)
			t.Cleanup(func() { _ = ln.Close() })
			h, err := r.Start(spec)
			if err != nil {
				t.Fatal(err)
			}
			if tc.stop {
				if err := r.Stop(h, spec.grace); err != nil {
					t.Fatalf("Stop: %v", err)
				}
			} else {
				select {
				case <-r.Wait(h):
				case <-time.After(10 * time.Second):
					t.Fatal("crash never observed")
				}
			}
			pollUntil(t, 3*time.Second, "the unit to remove its socket", func() bool {
				_, err := os.Lstat(spec.socket)
				return os.IsNotExist(err)
			})
		})
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
	ok := scriptApp(t, "echo \"$SOCKET $(pwd)\" > out.txt\nexit 0\n")
	if err := r.RunOnce(context.Background(), ok); err != nil {
		t.Fatalf("RunOnce ok: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(ok.dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(out))
	realDir, _ := filepath.EvalSymlinks(ok.dir)
	if len(fields) != 2 || fields[0] != "/var/lib/liveswap/itest/run/0a1b2c3d0a1b2c3d.sock" || fields[1] != realDir {
		t.Fatalf("env/cwd not propagated: %q (want the SOCKET path and %s)", out, realDir)
	}

	bad := scriptApp(t, "exit 4\n")
	if err := r.RunOnce(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "exit status 4") {
		t.Fatalf("non-zero exit: %v", err)
	}

	slow := scriptApp(t, "sleep 300\n")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err = r.RunOnce(ctx, slow)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled RunOnce: %v", err)
	}
	// A cancelled RunOnce stops its unit; the manager must not still
	// hold one (its pids are inside the unit's own namespace, so the
	// unit list is the host-side fact to check).
	deadline := time.Now().Add(5 * time.Second)
	for {
		units := strings.TrimSpace(run(t, "systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "hotserve-itest.runonce.*"))
		if units == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled RunOnce left a unit behind:\n%s", units)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestIntegrationSystemdReattachAdoptsLiveUnit(t *testing.T) {
	r1 := integrationRunner(t)
	spec := scriptApp(t, workerTree)
	h1, err := r1.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	readPIDs(t, spec.dir) // readiness: the workers are forked
	st := h1.state()
	pids := unitPIDs(t, st.Unit)
	if st.Unit == "" {
		t.Fatal("state must name the unit")
	}
	// The command is decoded out of the manager's own ExecStart
	// property — a nested D-Bus shape no fake can prove the layout of,
	// which is why the assertion belongs in this lane. A parser that
	// reads it as nothing would leave status quietly silent about what
	// every app is running.
	if len(st.Command) == 0 || !filepath.IsAbs(st.Command[0]) || filepath.Base(st.Command[0]) != "server" {
		t.Fatalf("the manager's ExecStart must decode to the resolved argv, got %v", st.Command)
	}
	// A "new hotserve": its own runner, same manager.
	r2 := newSystemdRunner(userManager, zap.NewNop())
	r2.poll = 50 * time.Millisecond
	t.Cleanup(r2.close)
	h2, ok, err := r2.Reattach(st)
	if !ok || err != nil {
		t.Fatalf("live unit must be adopted: ok=%v err=%v", ok, err)
	}
	if h2.state().PID != st.PID || !hasPID(pids, h2.state().PID) || h2.state().Unit != st.Unit {
		t.Fatalf("adopted %+v, want pid %d (in cgroup %v) unit %s", h2.state(), st.PID, pids, st.Unit)
	}
	// Reattach has no launch of its own to remember, so an adopted
	// handle can only report the command by reading it back — and must,
	// or status would go blank for every app across a hotserve restart.
	if !slices.Equal(h2.state().Command, st.Command) {
		t.Fatalf("adopted handle reports command %v, want the running unit's %v", h2.state().Command, st.Command)
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
	if _, ok, err := r2.Reattach(st); ok || err != nil {
		t.Fatalf("a stopped unit must not be adoptable: ok=%v err=%v", ok, err)
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
	if _, ok, err := r2.Reattach(h.state()); ok || err != nil {
		t.Fatalf("a failed unit must never be adopted: ok=%v err=%v", ok, err)
	}
	st, err := userManager.UnitStatus(context.Background(), h.state().Unit)
	if err != nil || st.loaded() {
		t.Fatalf("failed unit must be reset on discovery, got %+v (%v)", st, err)
	}
}

func TestIntegrationSystemdSweepStopsStrays(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, workerTree)
	keep, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	strayDir := t.TempDir()
	straySpec := spec
	straySpec.nonce = "deadbeefdeadbeef"
	straySpec.dir = strayDir
	straySpec.sandbox = itestSandbox(strayDir)
	if err := os.WriteFile(filepath.Join(strayDir, "server"), []byte("#!/bin/sh\n"+workerTree), 0o755); err != nil {
		t.Fatal(err)
	}
	stray, err := r.Start(straySpec)
	if err != nil {
		t.Fatal(err)
	}
	readPIDs(t, strayDir)
	readPIDs(t, spec.dir)
	strayPIDs := unitPIDs(t, stray.state().Unit)
	keepPIDs := unitPIDs(t, keep.state().Unit)
	if err := r.Sweep(spec.app, keep); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	waitPIDsGone(t, strayPIDs, 200*time.Millisecond)
	for _, p := range keepPIDs {
		if !alivePID(p) {
			t.Fatalf("keep's process %d was killed by the sweep", p)
		}
	}
	select {
	case <-r.Wait(stray):
	case <-time.After(5 * time.Second):
		t.Fatal("stray handle never saw its unit go")
	}
	if err := r.Stop(keep, spec.grace); err != nil {
		t.Fatal(err)
	}
}

// managerPID is the user manager's own process, for the stall test.
func managerPID(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("systemctl", "show", "-p", "MainPID", "--value", "user@"+strconv.Itoa(os.Getuid())+".service").Output()
	if err != nil {
		t.Fatalf("systemctl show user@: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid == 0 {
		t.Fatalf("no user manager pid: %q", out)
	}
	return pid
}

// The manager stops answering (SIGSTOP) for longer than a poll timeout:
// invariant 2 says the handle stays alive, and once the manager is
// back the watcher resumes and a Stop is honoured.
func TestIntegrationSystemdManagerStallIsNotACrash(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, workerTree)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	readPIDs(t, spec.dir) // readiness: the workers are forked
	pids := unitPIDs(t, h.state().Unit)
	mgr := managerPID(t)
	if err := syscall.Kill(mgr, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			_ = syscall.Kill(mgr, syscall.SIGCONT)
		}
	}
	defer resume()
	// Longer than pollTimeout: at least one poll must time out.
	deadline := time.Now().Add(pollTimeout + 3*time.Second)
	for time.Now().Before(deadline) {
		if !r.Alive(h) {
			t.Fatal("a stalled manager was reported as an instance exit")
		}
		time.Sleep(200 * time.Millisecond)
	}
	resume()
	if err := r.Stop(h, spec.grace); err != nil {
		t.Fatalf("Stop after the manager resumed: %v", err)
	}
	waitPIDsGone(t, pids, 2*time.Second)
}

// sandboxRoot makes a liveswap-style root outside /tmp (PrivateTmp=
// would hide a t.TempDir) with one app's release and shared dirs, a
// sibling app, and a state.json — the layout the sandbox must slice.
func sandboxRoot(t *testing.T) (root, release, shared string) {
	t.Helper()
	root, err := os.MkdirTemp("/var/tmp", "liveswap-itest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	release = filepath.Join(root, "itest", "releases", "v1")
	shared = filepath.Join(root, "itest", "shared")
	for _, d := range []string{release, shared, filepath.Join(root, "itest", "tmp"), filepath.Join(root, "other", "shared")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(root, "itest", "state.json"), filepath.Join(root, "other", "shared", "secret")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The convenience symlink an app really has next to its dirs.
	// Without it the probe's current=closed would be a check on a path
	// that never existed on the host either.
	if err := os.Symlink(filepath.Join("releases", "v1"), filepath.Join(root, "itest", "current")); err != nil {
		t.Fatal(err)
	}
	return root, release, shared
}

// TestIntegrationSystemdSandboxProbe: the capability probe against the
// real manager. The dev-systemd image is trixie (systemd 257) — the
// support matrix — so the answer is capable, unconditionally. A
// refusal here is a real failure and not a host to accommodate:
// either the kernel under the test container refuses user namespaces,
// or the sandbox regressed.
func TestIntegrationSystemdSandboxProbe(t *testing.T) {
	r := integrationRunner(t)
	if err := probeSandboxCapability(r); err != nil {
		t.Fatalf("the supported host must deliver the sandbox: %v", err)
	}
}

// writeSandboxView installs the shared view probe (sandboxViewScript,
// the same file e2e/liveswap/systemd.sh and packaging/test/smoke.sh
// use) into the release dir, alongside a ./server that sources it and
// then idles. Sourced, not run as a child, so the probe reports $$ as
// the unit's main pid.
func writeSandboxView(t *testing.T, release string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(release, sandboxViewName), []byte(sandboxViewScript), 0o644); err != nil {
		t.Fatal(err)
	}
	// sleep, not exec sleep: as namespace PID 1 an exec'd sleep would
	// have no default SIGTERM disposition and Stop would always have to
	// escalate to SIGKILL.
	server := "#!/bin/sh\n. ./sandbox-view.sh\nsleep 300\n"
	if err := os.WriteFile(filepath.Join(release, "server"), []byte(server), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedRuntimeDirFixture creates the runtime dir and admin socket that a
// real hotserve has, because this container runs none. Without it
// run_hotserve=closed and admin_socket=closed pass on paths that never
// existed on the host, proving nothing about the view — the same
// vacuity the manager-socket and /proc checks guard against by pinning
// the uid and the pid. The packaged unit creates /run/hotserve through
// RuntimeDirectory=; the socket is a real one, so [ -e ] sees what it
// would see in production.
func seedRuntimeDirFixture(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll("/run/hotserve", 0o750); err != nil {
		t.Fatalf("seeding /run/hotserve: %v", err)
	}
	sock := "/run/hotserve/admin.sock"
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("seeding %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = l.Close(); _ = os.Remove(sock) })
	// The probe's "closed" is only evidence if the path is here to hide.
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("%s was not created; the in-unit probe would prove nothing: %v", sock, err)
	}
}

func readProbe(t *testing.T, shared string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(shared, "view.txt"))
		if err == nil && strings.Contains(string(b), "done=1") {
			m := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				k, v, _ := strings.Cut(line, "=")
				m[k] = v
			}
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("view.txt not written: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestIntegrationSystemdSandboxedUnit starts a unit with the full
// sandbox and reads the view from inside: the namespaces are in
// effect, the root shows only the app's own dirs, everything nothing
// named is absent (not merely unreadable), the manager's /proc is
// closed, cgroupfs is read-only, /tmp is private and writable, HOME is
// the shared dir. Then the unit is stopped through the runner like any
// other.
func TestIntegrationSystemdSandboxedUnit(t *testing.T) {
	r := integrationRunner(t)
	root, release, shared := sandboxRoot(t)
	writeSandboxView(t, release)
	seedRuntimeDirFixture(t)
	mgrPID := strings.TrimSpace(run(t, "systemctl", "show", "-p", "MainPID", "--value", "user@"+strconv.Itoa(os.Getuid())+".service"))
	if mgrPID == "" || mgrPID == "0" {
		mgrPID = strconv.Itoa(os.Getppid())
	}
	spec := startSpec{
		app:     "itest",
		version: "sandboxed",
		nonce:   "0a1b2c3d0a1b2c3d",
		command: []string{"./server"},
		dir:     release,
		env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + shared,
			"MGR_PID=" + mgrPID, "HOTSERVE_UID=" + strconv.Itoa(os.Getuid())},
		grace: 2 * time.Second,
		sandbox: &sandboxSpec{root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}
	h, err := r.Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(h, 2*time.Second) })
	got := readProbe(t, shared)
	t.Logf("probe: %v", got)
	for k, want := range map[string]string{
		"pid": "1", "mgr_root": "closed", "mgr_environ": "closed",
		// The root shows this app and nothing else: sandboxRoot puts a
		// second app's shared/secret next door, and it must not be
		// nameable from in here.
		"root_listing": "itest ",
		"state":        "closed", "apptmp": "closed", "current": "closed",
		"release": "writable",
		// Deliberately no "root" expectation here. This lane's fixture
		// root is a /var/tmp temp dir, and PrivateTmp= gives the unit
		// its own writable /var/tmp with the binds created inside it
		// (sandbox.go, validateSandboxRoot) — so the root dir itself is
		// writable, while still naming nothing but this app. Under the
		// real /var/lib layout it is read-only, which e2e and the
		// packaging smoke both assert.
		"hotserve_lib": "closed", "run_hotserve": "closed", "etc_hotserve": "closed",
		"admin_socket": "closed", "mgr_socket": "closed",
		"cgroup": "readonly", "tmp": "writable", "home": shared, "xdg_runtime": "unset",
		// The base view: an OS the app can actually run on. Without
		// these, "absent" below would only mean the unit is empty.
		"binsh": "ok", "usrbinenv": "ok", "etcssl": "ok", "resolvconf": "ok",
		// The trust store is named, not the tree holding it: /etc/ssl
		// also holds /etc/ssl/private. This lane used to check only
		// that /etc/ssl existed, which a bind of the whole tree — every
		// app handed hotserve's TLS keys — would have satisfied.
		"sslprivate": "absent",
		// No name to resolve was supplied, and the probe says so rather
		// than omitting the key: e2e is where DNS from inside is proved.
		"dns": "skipped",
		// The probe echoes back what it was handed. A blank or literal
		// value here would make every /proc assertion above vacuous.
		"saw_mgr_pid": mgrPID, "saw_uid": strconv.Itoa(os.Getuid()),
		// And the uid it actually used for the manager-socket path:
		// /run/user/<wrong uid> is absent too, so mgr_socket=closed
		// only means something once this is pinned.
		"uid": strconv.Itoa(os.Getuid()),
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// Deny-by-default: not one of these is bound, so not one of them
	// exists — no InaccessiblePaths= entry, and no list to keep current.
	for _, k := range []string{
		"abs_varlib", "abs_etcliveswap", "abs_opt", "abs_srv",
		"abs_home", "abs_root", "abs_mnt", "abs_media",
	} {
		if got[k] != "absent" {
			t.Errorf("%s = %q, want absent", k, got[k])
		}
	}
	// /etc is named entry by entry, never bound whole: an app that could
	// list all of /etc would see every other app's env_file.
	for _, unwanted := range []string{"hotserve", "liveswap", "shadow", "sudoers"} {
		if strings.Contains(" "+got["etc_listing"], " "+unwanted+" ") {
			t.Errorf("/etc inside the unit contains %s: %q", unwanted, got["etc_listing"])
		}
	}
	if got["uidmap"] == "4294967295" {
		t.Error("no user namespace: uid_map covers the whole id space")
	}
	if n, _ := strconv.Atoi(got["nprocs"]); n == 0 || n > 8 {
		t.Errorf("nprocs = %q, want a handful (own PID namespace)", got["nprocs"])
	}
	unit := h.state().Unit
	props := run(t, "systemctl", "--user", "show", unit, "-p", "PrivatePIDs,PrivateUsers,ProtectSystem,ProtectControlGroups,TemporaryFileSystem,BindPaths,BindReadOnlyPaths,InaccessiblePaths")
	for _, want := range []string{"PrivatePIDs=yes", "PrivateUsers=yes", "ProtectControlGroups=yes", "TemporaryFileSystem=/:ro"} {
		if !strings.Contains(props, want) {
			t.Errorf("unit lacks %s:\n%s", want, props)
		}
	}
	// The retired half of the old model, read back off the live unit.
	for _, gone := range []string{"ProtectSystem=strict", "InaccessiblePaths=/"} {
		if strings.Contains(props, gone) {
			t.Errorf("unit still carries %s: the view names what exists, it does not mask a list\n%s", gone, props)
		}
	}
	if err := r.Stop(h, 2*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// run executes a command and returns its stdout, failing the test on
// error.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// TestIntegrationSystemdSandboxedUnitFailsAfterItsStartJobSucceeds pins
// the systemd semantics that decide WHERE a sandboxed unit's failure
// can be observed, and so what a synchronous caller is entitled to
// conclude from a start that returned cleanly.
//
// App units are Type=simple (unitProperties sets oneshot only for
// pre_start). A Type=simple start job completes as soon as the manager
// has forked — before the child sets its namespaces up and before it
// execs — so everything that can go wrong in the child arrives
// afterwards, through watch → finish, with the start job already
// reported "done".
//
// That is why the capability invalidation removed in this branch could
// not work: it hung off Start's and RunOnce's synchronous failure
// paths, and the failure it existed to catch (226/NAMESPACE, when a
// kernel or LSM change withdraws the namespaces under a live manager
// connection) never reaches them. A future round tempted to reinstate
// it should fail here first.
//
// A missing interpreter rather than broken namespaces: 203/EXEC and
// 226/NAMESPACE both come from the same forked child after the same
// "done", and this one needs no host surgery to provoke. unitFor's own
// in-view check passes — the script is executable and inside the
// sandbox view — so the failure genuinely happens in the child.
func TestIntegrationSystemdSandboxedUnitFailsAfterItsStartJobSucceeds(t *testing.T) {
	r := integrationRunner(t)
	root, release, shared := sandboxRoot(t)
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/nonexistent/interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := startSpec{
		app:     "itest",
		version: "asyncfail",
		nonce:   "0a1b2c3d0a1b2c3d",
		command: []string{"./server"},
		dir:     release,
		env:     []string{"PATH=" + os.Getenv("PATH"), "HOME=" + shared},
		grace:   2 * time.Second,
		sandbox: &sandboxSpec{root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}
	h, err := r.Start(spec)
	if err != nil {
		t.Fatalf("the start job reported this unit's failure synchronously: %v\n"+
			"If systemd now completes a Type=simple job only after exec, the asynchrony this pins is gone "+
			"and the sandbox capability cache could be invalidated from a launch path again.", err)
	}
	t.Cleanup(func() { _ = r.Stop(h, 2*time.Second) })

	// The unit is already doomed, and nothing synchronous saw it.
	select {
	case <-r.Wait(h):
	case <-time.After(30 * time.Second):
		t.Fatal("unit never reached a terminal state")
	}
	exit := h.(*systemdHandle).exit.Load()
	if exit == nil {
		t.Fatal("no exit recorded for a unit observed dead")
	}
	if exit.ExecMainStatus != 203 {
		t.Fatalf("ExecMainStatus = %d (%s), want 203/EXEC — the child died in exec, after the start job said done",
			exit.ExecMainStatus, exit.exitString())
	}
}

// TestIntegrationSystemdSIGTERMReachesNamespaceInit pins a property the
// full tier could plausibly have broken: with PrivatePIDs= the app is
// PID 1 of its own namespace, and the kernel discards signals sent to a
// namespace init from an ancestor namespace when the handler is SIG_DFL
// (the classic "docker stop takes ten seconds" behaviour). If that
// applied here, every cutover, drain and watchdog stop would silently
// wait out `grace` and end in SIGKILL,
// losing in-flight requests — the e2e cannot see it because its app is
// a Go binary, whose runtime installs handlers for every signal.
func TestIntegrationSystemdSIGTERMReachesNamespaceInit(t *testing.T) {
	r := integrationRunner(t)
	root, release, shared := sandboxRoot(t)
	// A shell that handles SIGTERM and takes its time about it, the way
	// a draining server does.
	body := "trap 'echo drained > " + shared + "/drained.txt; sleep 2; exit 0' TERM\nwhile :; do sleep 0.2; done\n"
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := startSpec{
		app: "itest", version: "sigterm", nonce: "0a1b2c3d0a1b2c3d", command: []string{"./server"}, dir: release,
		env: []string{"PATH=" + os.Getenv("PATH")}, grace: 10 * time.Second,
		sandbox: &sandboxSpec{root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}
	h, err := r.Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	started := time.Now()
	if err := r.Stop(h, spec.grace); err != nil {
		t.Fatalf("stop: %v", err)
	}
	took := time.Since(started)
	if _, err := os.Stat(filepath.Join(shared, "drained.txt")); err != nil {
		t.Fatalf("the app's SIGTERM handler never ran inside its PID namespace: %v", err)
	}
	if took >= spec.grace {
		t.Fatalf("stop took %s, the whole grace — SIGTERM was discarded and SIGKILL did the work", took)
	}
	t.Logf("SIGTERM honoured inside the PID namespace: handler ran, stop took %s of a %s grace", took, spec.grace)
}
