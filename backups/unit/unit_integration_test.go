//go:build integration

package unit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These run as root against a real system manager (make
// test-integration): every claim in the package doc is a measurement
// here, and none of them can be made of a fake.

// testUser runs what no test cares about the owner of.
const testUser = "hotserve-backup"

func runner(t *testing.T) *Runner {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root and a system manager")
	}
	ensureUser(t, "hotserve")
	ensureUser(t, testUser)
	r, err := NewSystemRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func ensureUser(t *testing.T, name string) {
	t.Helper()
	if exec.Command("id", name).Run() == nil {
		return
	}
	if out, err := exec.Command("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", name).CombinedOutput(); err != nil {
		t.Fatalf("useradd %s: %v: %s", name, err, out)
	}
}

func name(t *testing.T) string {
	n := strings.ToLower(strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	return "hotserve_backup_test_" + n + ".service"
}

// outFile is somewhere the manager can write and no unit can see.
func outFile(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/root", "unit-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "stdout")
}

func activeState(name string) string {
	out, _ := exec.Command("systemctl", "show", "-p", "ActiveState", "--value", name).Output()
	return strings.TrimSpace(string(out))
}

func TestIntegrationExitStatusIsTheOutcomeNotAnError(t *testing.T) {
	r := runner(t)
	out, err := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/sh", "-c", "exit 12"}, User: testUser})
	if err != nil {
		t.Fatalf("a command that exits 12 is an outcome, got error %v", err)
	}
	if out.ExitStatus != 12 || out.Result != "exit-code" || out.OK() {
		t.Fatalf("outcome = %+v, want exit status 12, result exit-code", out)
	}
	if st := activeState(name(t)); st == "failed" {
		t.Fatalf("the unit is left in the failed state")
	}
	ok, err := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/true"}, User: testUser})
	if err != nil || !ok.OK() {
		t.Fatalf("a clean exit under the same name: %+v, %v", ok, err)
	}
}

func TestIntegrationArgvIsNeverExpanded(t *testing.T) {
	r := runner(t)
	stdout := outFile(t)
	out, err := r.Run(context.Background(), Spec{
		Name: name(t), Argv: []string{"/bin/echo", "${SECRET}", "$SECRET", "$$"}, User: testUser,
		Environment: []string{"SECRET=the-repository-password"}, StdoutFile: stdout,
	})
	if err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	got, _ := os.ReadFile(stdout)
	if string(got) != "${SECRET} $SECRET $$\n" {
		t.Fatalf("the command received %q", got)
	}
}

func TestIntegrationBindSourceIsAPathAndNothingElse(t *testing.T) {
	r := runner(t)
	dir := filepath.Join(t.TempDir(), "live swap:50%$HOME", "blog", "shared")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// TempDir is under /tmp, which PrivateTmp hides from the unit but
	// not from the manager resolving the bind source.
	os.WriteFile(filepath.Join(dir, "row"), []byte("row\n"), 0o644)
	for p := dir; p != "/tmp" && p != "/"; p = filepath.Dir(p) {
		os.Chmod(p, 0o755)
	}
	stdout := outFile(t)
	out, err := r.Run(context.Background(), Spec{
		Name: name(t), Argv: []string{"/bin/cat", "/backup/blog/files/row"}, User: testUser,
		Binds: []Bind{{Source: dir, Dest: "/backup/blog/files"}}, StdoutFile: stdout,
	})
	if err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	if got, _ := os.ReadFile(stdout); string(got) != "row\n" {
		t.Fatalf("read %q through the bind", got)
	}
}

// For every way a Spec can say who runs the command: a property that
// quietly widens the view (DynamicUser= does) is found here.
func TestIntegrationViewIsWhatIsNamed(t *testing.T) {
	r := runner(t)
	// /run/systemd/incoming is the manager's own: the directory mounts
	// propagate into the namespace through, closed to the unit. Nothing
	// else of /run/systemd — the manager's sockets least of all — is
	// there unless a Spec names it.
	script := `for p in /etc/shadow /root /home /var/lib /etc/hotserve /run/dbus /run/systemd/private /run/systemd/journal; do [ -e "$p" ] && echo "present: $p"; done
ls /run/systemd | grep -v -x -e incoming -e resolve | sed 's/^/present: \/run\/systemd\//'; echo done`
	for who, spec := range map[string]Spec{
		"a user":                   {User: testUser},
		"a user with a capability": {User: testUser, Capabilities: []Capability{CapDACReadSearch}},
		"a user in its namespaces": {User: "hotserve", SameUIDNamespaces: true},
		"a user with the network":  {User: testUser, Network: true},
		"root with one capability": {AsRoot: true, Capabilities: []Capability{CapChown}},
	} {
		t.Run(who, func(t *testing.T) {
			spec.Name, spec.Argv, spec.StdoutFile = name(t), []string{"/bin/sh", "-c", script}, outFile(t)
			out, err := r.Run(context.Background(), spec)
			if err != nil || !out.OK() {
				t.Fatalf("%+v, %v", out, err)
			}
			got, _ := os.ReadFile(spec.StdoutFile)
			if string(got) != "done\n" {
				t.Fatalf("the view holds more than was named:\n%s", got)
			}
		})
	}
}

// A private network namespace here still holds lo and the kernel's
// fallback tunnel devices, so what is counted is routes: none, against
// the host's at least one.
func TestIntegrationNoNetworkUnlessAsked(t *testing.T) {
	r := runner(t)
	routes := func(network bool) string {
		stdout := outFile(t)
		n := strings.TrimSuffix(name(t), ".service") + map[bool]string{true: "_with", false: "_without"}[network] + ".service"
		out, err := r.Run(context.Background(), Spec{Name: n, Argv: []string{"/bin/sh", "-c", "tail -n +2 /proc/net/route | wc -l"}, User: testUser, Network: network, StdoutFile: stdout})
		if err != nil || !out.OK() {
			t.Fatalf("%+v, %v", out, err)
		}
		got, _ := os.ReadFile(stdout)
		return strings.TrimSpace(string(got))
	}
	if got := routes(false); got != "0" {
		t.Fatalf("a unit that did not ask for the network has %s route(s)", got)
	}
	if got := routes(true); got == "0" || got == "" {
		t.Fatalf("a unit that asked for the network has %q routes", got)
	}
}

func TestIntegrationMaskedPathsReadEmptyAndAMissingOneIsSkipped(t *testing.T) {
	r := runner(t)
	dir, _ := os.MkdirTemp("/root", "unit-mask-")
	t.Cleanup(func() { os.RemoveAll(dir) })
	os.Chmod(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "app.db"), []byte("LIVE DATABASE BYTES"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.png"), []byte("img"), 0o644)
	stdout := outFile(t)
	out, err := r.Run(context.Background(), Spec{
		Name: name(t), User: testUser, StdoutFile: stdout,
		Argv:   []string{"/bin/sh", "-c", "cat /backup/x/files/app.db; echo \"|\"; cat /backup/x/files/a.png"},
		Binds:  []Bind{{Source: dir, Dest: "/backup/x/files"}},
		Masked: []string{"/backup/x/files/app.db", "/backup/x/files/app.db-wal"},
	})
	if err != nil {
		t.Fatalf("%+v, %v", out, err)
	}
	if got, _ := os.ReadFile(stdout); string(got) != "|\nimg" {
		t.Fatalf("through the mask: %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("masking created something in the real directory: %v", entries)
	}
}

func TestIntegrationCapabilityReadsAnotherUsersFileAndWritesNothing(t *testing.T) {
	r := runner(t)
	ensureUser(t, "hotserve")
	ensureUser(t, "hotserve-backup")
	dir, _ := os.MkdirTemp("/root", "unit-cap-")
	t.Cleanup(func() { os.RemoveAll(dir) })
	if out, err := exec.Command("sh", "-c", "chmod 0755 "+dir+" && install -d -o hotserve -g hotserve -m 0700 "+dir+"/shared && install -o hotserve -g hotserve -m 0600 /dev/null "+dir+"/shared/secret && echo private > "+dir+"/shared/secret").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	spec := func(caps []Capability) Spec {
		return Spec{
			Name: name(t), User: "hotserve-backup", Capabilities: caps, StdoutFile: outFile(t),
			Argv:  []string{"/bin/sh", "-c", "cat /backup/x/files/secret; touch /backup/x/files/new 2>/dev/null && echo WROTE; true"},
			Binds: []Bind{{Source: dir + "/shared", Dest: "/backup/x/files"}},
		}
	}
	with := spec([]Capability{CapDACReadSearch})
	if out, err := r.Run(context.Background(), with); err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	if got, _ := os.ReadFile(with.StdoutFile); string(got) != "private\n" {
		t.Fatalf("with the capability: %q", got)
	}
	without := spec(nil)
	if _, err := r.Run(context.Background(), without); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(without.StdoutFile); string(got) != "" {
		t.Fatalf("without the capability the file is still readable: %q", got)
	}
}

func TestIntegrationSameUIDNamespaces(t *testing.T) {
	r := runner(t)
	ensureUser(t, "hotserve")
	stdout := outFile(t)
	script := `read _ _ n < /proc/self/uid_map; echo "range=$n pid=$$"`
	out, err := r.Run(context.Background(), Spec{Name: name(t), Argv: []string{"/bin/sh", "-c", script}, User: "hotserve", SameUIDNamespaces: true, StdoutFile: stdout})
	if err != nil || !out.OK() {
		t.Fatalf("%+v, %v", out, err)
	}
	got, _ := os.ReadFile(stdout)
	if strings.Contains(string(got), "range=4294967295") || !strings.Contains(string(got), "pid=1\n") {
		t.Fatalf("not in its own user and PID namespaces: %q", got)
	}
}

// A real process that outlives the context, because only that shows
// what cancelling does: Run must not return until the unit is gone.
func TestIntegrationCancelStopsTheUnitAndConfirmsItGone(t *testing.T) {
	r := runner(t)
	ctx, cancel := context.WithCancel(context.Background())
	n := name(t)
	go func() {
		for activeState(n) != "activating" {
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()
	_, err := r.Run(ctx, Spec{Name: n, Argv: []string{"/bin/sleep", "600"}, User: testUser})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotConfirmedGone) {
		t.Fatalf("want the context's error and a confirmed stop, got %v", err)
	}
	if st := activeState(n); st != "inactive" {
		t.Fatalf("after Run returned the unit is %q", st)
	}
	if out, _ := exec.Command("pgrep", "-f", "^/bin/sleep 600$").Output(); len(out) != 0 {
		t.Fatalf("the process is still running: %s", out)
	}
}

func waitFor(t *testing.T, unit, state string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); activeState(unit) != state; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s is %q, not %q, after 20s", unit, activeState(unit), state)
		}
	}
}

// The orchestrator dies without a chance to clean up; the manager ends
// what it started, and Run says the command gave no verdict.
func TestIntegrationBindsToEndsTheUnitWhenItsOrchestratorIsKilled(t *testing.T) {
	r := runner(t)
	orch := "hotserve_backup_test_orchestrator.service"
	t.Cleanup(func() { exec.Command("systemctl", "stop", orch).Run() })
	go r.Run(context.Background(), Spec{Name: orch, Argv: []string{"/bin/sleep", "601"}, User: testUser})
	waitFor(t, orch, "activating")
	n := name(t)
	t.Cleanup(func() { exec.Command("systemctl", "stop", n).Run() })
	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := r.Run(context.Background(), Spec{Name: n, Argv: []string{"/bin/sleep", "602"}, User: testUser, BindsTo: orch})
		done <- result{out, err}
	}()
	waitFor(t, n, "activating")
	if out, err := exec.Command("systemctl", "kill", "--signal=SIGKILL", orch).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	select {
	case res := <-done:
		if res.err == nil || !strings.Contains(res.err.Error(), "ended from outside") {
			t.Fatalf("want an error saying the unit was ended from outside, got %+v, %v", res.out, res.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the bound unit is still running 30s after its orchestrator was killed")
	}
	// Run has returned, so the process is gone — not about to be.
	if out, _ := exec.Command("pgrep", "-f", "^/bin/sleep 602$").Output(); len(out) != 0 {
		t.Fatalf("the process is still running: %s", out)
	}
}
