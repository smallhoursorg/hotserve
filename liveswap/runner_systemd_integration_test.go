//go:build integration

package liveswap

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
	h2, ok, err := r2.Reattach(st)
	if !ok || err != nil {
		t.Fatalf("live unit must be adopted: ok=%v err=%v", ok, err)
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
	straySpec.dir = strayDir
	if err := os.WriteFile(filepath.Join(strayDir, "server"), []byte("#!/bin/sh\n"+workerTree), 0o755); err != nil {
		t.Fatal(err)
	}
	stray, err := r.Start(straySpec)
	if err != nil {
		t.Fatal(err)
	}
	strayPIDs := readPIDs(t, strayDir)
	keepPIDs := readPIDs(t, spec.dir)
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
	pids := readPIDs(t, spec.dir)
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
	return root, release, shared
}

// TestIntegrationSystemdSandboxProbe: the capability probe against the
// real manager. The dev-systemd image is trixie (systemd 257) — the
// support matrix — so the answer is full, unconditionally. A `none`
// here is a real failure and not a host to accommodate: either the
// kernel under the test container refuses user namespaces, or the
// sandbox regressed.
func TestIntegrationSystemdSandboxProbe(t *testing.T) {
	r := integrationRunner(t)
	got := probeSandboxCapability(r)
	t.Logf("tier=%s reason=%q", got.tier, got.reason)
	if got.tier != sandboxFull {
		t.Fatalf("the supported host must deliver the full tier, got %s (%s)", got.tier, got.reason)
	}
}

// sandboxProbeScript writes one line per check into $1 (the shared
// dir, the only writable persistent path in the view). MGR and HS are
// the user manager's and a bystander's PIDs.
const sandboxProbeScript = `out="$1/probe.txt"; MGR=$2; ROOT=$3
: > "$out"
echo "pid=$$" >> "$out"
read _ _ n < /proc/self/uid_map; echo "uidmap=$n" >> "$out"
echo "nprocs=$(ls /proc | grep -c '^[0-9]')" >> "$out"
ls /proc/$MGR/root/ >/dev/null 2>&1 && echo "mgr_root=open" >> "$out" || echo "mgr_root=closed" >> "$out"
cat /proc/$MGR/environ >/dev/null 2>&1 && echo "mgr_environ=open" >> "$out" || echo "mgr_environ=closed" >> "$out"
[ -e "$ROOT/other" ] && echo "sibling=open" >> "$out" || echo "sibling=closed" >> "$out"
[ -e "$ROOT/itest/state.json" ] && echo "state=open" >> "$out" || echo "state=closed" >> "$out"
[ -e "$ROOT/itest/tmp" ] && echo "apptmp=open" >> "$out" || echo "apptmp=closed" >> "$out"
touch "$ROOT/itest/releases/v1/w" 2>/dev/null && echo "release=writable" >> "$out" || echo "release=readonly" >> "$out"
touch "$ROOT/newfile" 2>/dev/null && echo "root=writable" >> "$out" || echo "root=readonly" >> "$out"
# Absence, not unreadability: under a deny-by-default view an unnamed
# path does not exist inside the unit, which is a stronger statement
# than the InaccessiblePaths= node this used to test for readability.
[ -e /var/lib/hotserve ] && echo "hotserve_lib=open" >> "$out" || echo "hotserve_lib=closed" >> "$out"
[ -e /run/user/$(id -u)/systemd/private ] && echo "mgr_socket=open" >> "$out" || echo "mgr_socket=closed" >> "$out"
for d in /var/lib /etc/hotserve /etc/liveswap /run/hotserve /opt /srv /home /root /mnt; do
  k=$(echo "$d" | tr -d /); [ -e "$d" ] && echo "abs_$k=present" >> "$out" || echo "abs_$k=absent" >> "$out"
done
echo "etc_listing=$(ls /etc | tr '\n' ' ')" >> "$out"
# And the base view must actually carry a runnable OS, or "absent"
# would only mean the unit has nothing at all.
[ -x /bin/sh ] && echo "binsh=ok" >> "$out" || echo "binsh=MISSING" >> "$out"
[ -x /usr/bin/env ] && echo "usrbinenv=ok" >> "$out" || echo "usrbinenv=MISSING" >> "$out"
[ -e /etc/ssl ] && echo "etcssl=ok" >> "$out" || echo "etcssl=MISSING" >> "$out"
[ -r /etc/resolv.conf ] && echo "resolvconf=ok" >> "$out" || echo "resolvconf=MISSING" >> "$out"
cg=/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup); (echo max > "$cg/memory.max") 2>/dev/null && echo "cgroup=writable" >> "$out" || echo "cgroup=readonly" >> "$out"
touch /tmp/w 2>/dev/null && echo "tmp=writable" >> "$out" || echo "tmp=readonly" >> "$out"
echo "home=$HOME" >> "$out"
[ -n "$XDG_RUNTIME_DIR" ] && echo "xdg_runtime=set" >> "$out" || echo "xdg_runtime=unset" >> "$out"
echo "done=1" >> "$out"
sleep 300
`

func readProbe(t *testing.T, shared string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(shared, "probe.txt"))
		if err == nil && strings.Contains(string(b), "done=1") {
			m := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				k, v, _ := strings.Cut(line, "=")
				m[k] = v
			}
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe.txt not written: %v", err)
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
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\n"+sandboxProbeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	mgrPID := strings.TrimSpace(run(t, "systemctl", "show", "-p", "MainPID", "--value", "user@"+strconv.Itoa(os.Getuid())+".service"))
	if mgrPID == "" || mgrPID == "0" {
		mgrPID = strconv.Itoa(os.Getppid())
	}
	spec := startSpec{
		app:     "itest",
		version: "sandboxed",
		command: []string{"./server", shared, mgrPID, root},
		dir:     release,
		env:     []string{"PATH=" + os.Getenv("PATH"), "HOME=" + shared},
		grace:   2 * time.Second,
		sandbox: &sandboxSpec{tier: sandboxFull, root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
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
		"sibling": "closed", "state": "closed", "apptmp": "closed",
		"release": "writable", "hotserve_lib": "closed", "mgr_socket": "closed",
		"cgroup": "readonly", "tmp": "writable", "home": shared, "xdg_runtime": "unset",
		// The base view: an OS the app can actually run on. Without
		// these, "absent" below would only mean the unit is empty.
		"binsh": "ok", "usrbinenv": "ok", "etcssl": "ok", "resolvconf": "ok",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// Deny-by-default: not one of these is bound, so not one of them
	// exists — no InaccessiblePaths= entry, and no list to keep current.
	for _, k := range []string{
		"abs_varlib", "abs_etchotserve", "abs_etcliveswap", "abs_runhotserve",
		"abs_opt", "abs_srv", "abs_home", "abs_root", "abs_mnt",
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
	if h.state().Sandbox != "full" {
		t.Errorf("handle state sandbox = %q", h.state().Sandbox)
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
		app: "itest", version: "sigterm", command: []string{"./server"}, dir: release,
		env: []string{"PATH=" + os.Getenv("PATH")}, grace: 10 * time.Second,
		sandbox: &sandboxSpec{tier: sandboxFull, root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
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
