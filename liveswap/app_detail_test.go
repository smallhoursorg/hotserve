package liveswap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeJournal scripts what the app's units wrote.
type fakeJournal struct {
	lines []string
	err   error
	calls int
	of    tailOf
	since time.Time
	n     int
}

func (j *fakeJournal) tail(_ context.Context, of tailOf, since time.Time, n int) ([]string, error) {
	j.calls++
	j.of, j.since, j.n = of, since, n
	return j.lines, j.err
}

func deployOnceV1(t *testing.T, rig *testRig) error {
	t.Helper()
	return rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v1.tgz", version: "v1", by: "test"})
}

// A pre_start that exits non-zero: the response carries its exit
// status, the phases up to "preparing", and the last lines its unit
// wrote, and nothing about a health probe.
func TestFailureDetailPreStartExit(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}
	rig.spec.deployLogLines = 40
	rig.runner.runOnceErr = runOnceExit("exit status 3", "hotserve-demo.v1.x.prestart.service", "failed", 731)
	j := &fakeJournal{lines: []string{"migrate: schema v1 -> v2", "migrate: cannot open app.db"}}
	rig.ma.journal = j

	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed in pre_start")
	}
	ld := rig.ma.status().LastDeploy
	if ld == nil || ld.Status != "failed" || ld.Phase != "preparing" {
		t.Fatalf("last deploy = %+v", ld)
	}
	if ld.Detail == nil || ld.Detail.Exit != "exit status 3" || ld.Detail.Probe != nil {
		t.Fatalf("detail = %+v", ld.Detail)
	}
	if strings.Join(ld.Detail.LogTail, "|") != "migrate: schema v1 -> v2|migrate: cannot open app.db" || ld.Detail.LogTailTruncated {
		t.Fatalf("log tail = %v (truncated %v)", ld.Detail.LogTail, ld.Detail.LogTailTruncated)
	}
	// Asked twice: once, and once more to see the count stop growing.
	if j.calls != 2 || j.n != 40 || len(j.of.units) != 2 || !strings.HasSuffix(j.of.units[0], ".prestart.service") || !strings.HasSuffix(j.of.units[1], ".service") {
		t.Fatalf("journal asked %d times for %d lines of %v", j.calls, j.n, j.of.units)
	}
	if !j.since.Equal(ld.StartedAt) {
		t.Fatalf("journal since %v, deploy started %v", j.since, ld.StartedAt)
	}
	// And by the pre_start's main process, the pid the manager recorded,
	// under the app's identifier: journald keeps no unit on a line it
	// reads after its writer has gone.
	if len(j.of.pids) != 1 || j.of.pids[0] != 731 || j.of.ident != "hotserve-demo" {
		t.Fatalf("journal asked by pids %v under %q, want [731] under hotserve-demo", j.of.pids, j.of.ident)
	}
	var names []string
	for _, p := range ld.Phases {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "downloading,extracting,preparing" {
		t.Fatalf("phases = %s", got)
	}
}

// An app that dies before it is healthy: the exit comes from the
// runner, since no error carried it.
func TestFailureDetailAppDiesOnStart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.runner.startDies = "killed by signal 9 (killed)"
	rig.prober.err = &healthGateError{err: errProcessExited, probe: &probeError{status: 500, body: "booting"}}
	j := &fakeJournal{lines: []string{"fatal: SOCKET not set"}}
	rig.ma.journal = j

	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed the health gate")
	}
	ld := rig.ma.status().LastDeploy
	// The exit is the app's own, and the probe it answered before it
	// died is kept alongside.
	if ld.Detail == nil || ld.Detail.Exit != "killed by signal 9 (killed)" || ld.Detail.Probe == nil || ld.Detail.Probe.Status != 500 {
		t.Fatalf("detail = %+v", ld.Detail)
	}
	if len(ld.Detail.LogTail) != 1 || ld.Detail.LogTail[0] != "fatal: SOCKET not set" {
		t.Fatalf("log tail = %v", ld.Detail.LogTail)
	}
	// The journal is asked by the pid the app's main process had.
	if len(j.of.pids) != 1 || j.of.pids[0] != 4242 {
		t.Fatalf("journal asked by pids %v, want the app's [4242]", j.of.pids)
	}
}

// A gate that fails before any launch — the sweep that precedes
// pre_start could not be confirmed — has no units to ask about: no
// detail, and the journal is never run.
func TestFailureDetailNoneBeforeLaunch(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.sweepErr = errors.New("manager unreachable")
	j := &fakeJournal{lines: []string{"never"}}
	rig.ma.journal = j
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed before pre_start")
	}
	if d := rig.ma.status().LastDeploy.Detail; d != nil || j.calls != 0 {
		t.Fatalf("detail before any launch: %+v (journal calls %d)", d, j.calls)
	}
}

// More lines than deploy_log_lines: the tail is the last N and says
// there was more; a single oversized line is clipped, not dropped.
func TestFailureDetailCaps(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 2
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = runOnceExit("exit status 1", "u", "failed", 0)
	rig.ma.journal = &fakeJournal{lines: []string{"one", "two", "three"}} // journalctl asked for 3, returned 3
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed")
	}
	d := rig.ma.status().LastDeploy.Detail
	if strings.Join(d.LogTail, ",") != "two,three" || !d.LogTailTruncated {
		t.Fatalf("capped tail = %v (truncated %v)", d.LogTail, d.LogTailTruncated)
	}
}

// A health gate that never passes: the last probe's status, redirect
// target and body excerpt are in the detail; the process is alive, so
// there is no exit.
func TestFailureDetailProbe(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.prober.err = fmt.Errorf("not healthy within deadline 5m: %w", &probeError{status: 400, location: "", body: "Invalid HTTP_HOST header: 'localhost'."})
	rig.ma.journal = &fakeJournal{}

	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed the health gate")
	}
	ld := rig.ma.status().LastDeploy
	if ld.Detail == nil || ld.Detail.Probe == nil || ld.Detail.Probe.Status != 400 || !strings.Contains(ld.Detail.Probe.Body, "HTTP_HOST") {
		t.Fatalf("detail = %+v", ld.Detail)
	}
	if ld.Detail.Exit != "" {
		t.Fatalf("a stopped-by-hotserve instance must not report an exit as the cause: %q", ld.Detail.Exit)
	}
	if ld.Detail.LogTail != nil || ld.Detail.LogTailError != "" {
		t.Fatalf("an empty journal is no tail and no error: %+v", ld.Detail)
	}
}

// deploy_log_lines 0 asks the journal for nothing; a journal that
// cannot be read says so instead of a tail; and a success carries no
// detail at all.
func TestFailureDetailSwitches(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 0
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = runOnceExit("exit status 1", "u", "failed", 0)
	j := &fakeJournal{lines: []string{"never read"}}
	rig.ma.journal = j
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed")
	}
	if d := rig.ma.status().LastDeploy.Detail; j.calls != 0 || d == nil || d.LogTail != nil || d.Exit != "exit status 1" {
		t.Fatalf("with deploy_log_lines 0: journal calls %d, detail %+v", j.calls, d)
	}

	rig = newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = runOnceExit("exit status 1", "u", "failed", 0)
	rig.ma.journal = &fakeJournal{err: errors.New("journalctl: not found")}
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed")
	}
	if d := rig.ma.status().LastDeploy.Detail; d == nil || d.LogTailError != "journalctl: not found" || d.LogTail != nil {
		t.Fatalf("unreadable journal: %+v", d)
	}

	rig = newTestRig(t)
	rig.spec.deployLogLines = 40
	rig.ma.journal = &fakeJournal{lines: []string{"hello"}}
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	ld := rig.ma.status().LastDeploy
	if ld.Detail != nil {
		t.Fatalf("a success carries no detail: %+v", ld.Detail)
	}
	if len(ld.Phases) == 0 || ld.Phases[len(ld.Phases)-1].Name != "promoting" && ld.Phases[len(ld.Phases)-1].Name != "stopping_old" {
		t.Fatalf("phases = %+v", ld.Phases)
	}
}

// The tail's two runs, argument by argument: the streams are learnt
// from the lines that name the launch — by a unit, or by a main
// process's pid under the app's identifier — and the tail is the units'
// lines and every line of those streams.
func TestJournalArgs(t *testing.T) {
	since := time.Unix(1790000000, 0)
	window := "--user --no-pager --quiet -n 41 --since @1789999999"
	units := "_SYSTEMD_USER_UNIT=a.prestart.service _SYSTEMD_USER_UNIT=a.service"
	of := tailOf{units: []string{"a.prestart.service", "a.service"}, pids: []int{0, 731}, ident: "hotserve-demo"}
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"streams, by unit or by pid", streamArgs(of, since, 40), strings.Fields(window + " -o json --output-fields=_STREAM_ID " + units + " + _PID=731 SYSLOG_IDENTIFIER=hotserve-demo")},
		{"streams, no pid recorded", streamArgs(tailOf{units: of.units, pids: []int{0}, ident: of.ident}, since, 40), strings.Fields(window + " -o json --output-fields=_STREAM_ID " + units)},
		{"streams, no identifier to go with the pid", streamArgs(tailOf{units: of.units, pids: []int{731}}, since, 40), strings.Fields(window + " -o json --output-fields=_STREAM_ID " + units)},
		{"tail, the units or their streams", tailArgs(of.units, []string{"0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"}, since, 40), strings.Fields(window + " -o cat " + units + " + _STREAM_ID=0123456789abcdef0123456789abcdef _STREAM_ID=fedcba9876543210fedcba9876543210")},
		{"tail, no stream learnt", tailArgs(of.units, nil, since, 40), strings.Fields(window + " -o cat " + units)},
	} {
		if strings.Join(c.got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}
}

// The stream ids are read off journalctl's JSON, each once, in the
// order met; what is not one, as journald prints one, is passed over.
func TestStreamIDs(t *testing.T) {
	a, b := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	out := strings.Join([]string{
		`ID":"` + a + `"}`, // the first entry, its start cut by the byte bound
		`{"__CURSOR":"s=1","_STREAM_ID":"` + a + `"}`,
		`{"__CURSOR":"s=2"}`, // a line sent to the journal directly: no stream
		`{"_STREAM_ID":"` + b + `"}`,
		`{"_STREAM_ID":"` + a + `"}`, // met again
		`{"_STREAM_ID":"0123456789ABCDEF0123456789ABCDEF"}`,
		`{"_STREAM_ID":"x _PID=1"}`,
		`{"_STREAM_ID":[1,2]}`, // a field given twice
		"",
	}, "\n")
	if got := strings.Join(streamIDs([]byte(out)), ","); got != a+","+b {
		t.Fatalf("stream ids = %q, want %q", got, a+","+b)
	}
}

func TestCapTail(t *testing.T) {
	lines := []string{"a", "bb", "ccc", "dddd"}
	if got, tr := capTail(lines, 2, 1000); strings.Join(got, ",") != "ccc,dddd" || !tr {
		t.Errorf("line cap: %v %v", got, tr)
	}
	if got, tr := capTail(lines, 10, 9); strings.Join(got, ",") != "ccc,dddd" || !tr { // 4+1 + 3+1 = 9 fits; 2+1 more does not
		t.Errorf("byte cap: %v %v", got, tr)
	}
	if got, tr := capTail(lines, 10, 1000); len(got) != 4 || tr {
		t.Errorf("no cap: %v %v", got, tr)
	}
	if got, tr := capTail(nil, 10, 1000); got != nil || tr {
		t.Errorf("empty: %v %v", got, tr)
	}
	if got, tr := capTail([]string{strings.Repeat("x", 100)}, 10, 50); len(got) != 1 || !strings.HasPrefix(got[0], "…") || len(got[0]) > 50 || !tr {
		t.Errorf("one line over the byte cap is clipped, not dropped: %v %v", got, tr)
	}
	if got, tr := capTail([]string{"short", strings.Repeat("y", 100)}, 10, 50); len(got) != 1 || !strings.HasPrefix(got[0], "…") || !tr {
		t.Errorf("an oversized last line after others: %v %v", got, tr)
	}
}

// deploy_log_lines 0 keeps the app's output on the box: the probe's
// status and redirect target are reported, its body is not.
func TestFailureDetailZeroKeepsProbeBodyOnTheBox(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.deployLogLines = 0
	rig.prober.err = fmt.Errorf("not healthy within deadline 5m: %w", &probeError{status: 500, body: "Traceback (most recent call last): secret things"})
	rig.ma.journal = &fakeJournal{lines: []string{"never read"}}
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("deploy should have failed the health gate")
	}
	d := rig.ma.status().LastDeploy.Detail
	if d == nil || d.Probe == nil || d.Probe.Status != 500 || d.Probe.Body != "" {
		t.Fatalf("with deploy_log_lines 0 the probe body stays on the box: %+v", d)
	}
}

// tailWriter keeps the last max bytes whatever is written, in one
// write or many, and never more.
func TestTailWriter(t *testing.T) {
	w := &tailWriter{max: 8}
	for _, s := range []string{"abc", "def", "ghi"} {
		if n, err := w.Write([]byte(s)); n != 3 || err != nil {
			t.Fatalf("write %q: %d %v", s, n, err)
		}
	}
	if string(w.buf) != "bcdefghi" {
		t.Fatalf("after three writes: %q", w.buf)
	}
	if _, err := w.Write([]byte("0123456789ab")); err != nil || string(w.buf) != "456789ab" {
		t.Fatalf("one write over max: %q %v", w.buf, err)
	}
	if _, err := w.Write([]byte("xyz")); err != nil || string(w.buf) != "789abxyz" {
		t.Fatalf("a write after that: %q %v", w.buf, err)
	}
}

// The clipped oversized line is cut on a rune boundary.
func TestCapTailClipsOnRuneBoundary(t *testing.T) {
	line := strings.Repeat("é", 100) // 200 bytes, every one inside a 2-byte rune
	got, tr := capTail([]string{line}, 10, 50)
	if !tr || len(got) != 1 || !utf8.ValidString(got[0]) || len(got[0]) > 50 {
		t.Fatalf("clipped line must be valid UTF-8 within the cap: %q %v", got, tr)
	}
}
