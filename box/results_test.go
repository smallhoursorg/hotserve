package box

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
)

// The worst result the caps allow fits the handler's 2 MiB read cap with
// its diff and apps intact: a 64 KiB diff of bytes JSON escapes sixfold,
// the most app names a 1 MiB file can hold (`app a` lines, six bytes
// each), a 4 KiB path of bytes that are not UTF-8, a long error.
func TestResultFitsTheHandlersCap(t *testing.T) {
	apps := make([]string, (1<<20)/6)
	for i := range apps {
		apps[i] = "a"
	}
	r := result{
		ID: strings.Repeat("a", 32), Phase: phaseRolledBack, Commit: strings.Repeat("c", 40),
		Path: strings.Repeat("\xff", 4096), Signer: strings.Repeat("s", 256), BoxWebhook: strings.Repeat("h", 253),
		Apps: apps, Diff: strings.Repeat("\x01", maxDiff), Error: strings.Repeat("\"", 4096),
	}
	b := r.marshal()
	if len(b) > maxResult {
		t.Fatalf("%d bytes", len(b))
	}
	if bytes.Contains(b, []byte("omitted")) {
		t.Error("the diff gave way inside the caps")
	}
	// Past the caps, the verdict survives and the extras give way.
	r.Apps = slices.Concat(apps, apps, apps, apps)
	if b := r.marshal(); len(b) > maxResult || !bytes.Contains(b, []byte(`"phase":"rolled_back"`)) {
		t.Errorf("%d bytes", len(b))
	}
}

// A result that cannot be written goes to the journal whole, at error
// level (I3).
func TestResultJournaledWhenUnwritable(t *testing.T) {
	b := newTestBox(t)
	head := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	id := b.push(b.repo.bundleFiles(head, b.base))
	if err := b.run(hooks{fail: failAt(map[string]int{"result:applied": -1})}); err != nil {
		t.Fatal(err)
	}
	lines := b.logged(zapcore.ErrorLevel, "box result could not be written")
	if len(lines) != 1 {
		t.Fatalf("%d lines", len(lines))
	}
	fields := lines[0].ContextMap()
	for _, k := range []string{"id", "phase", "commit", "path", "signer", "box", "caddyfile_edited_out_of_band", "apps", "diff", "error", "write_error"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("the line has no %s", k)
		}
	}
	if fields["id"] != id || fields["phase"] != phaseApplied || fields["commit"] != head {
		t.Errorf("%v", fields)
	}
}

func TestRunApplyRefusesUnlessRoot(t *testing.T) {
	b := newTestBox(t)
	if err := runApply(context.Background(), 1000, b.applier(hooks{})); err == nil || !strings.Contains(err.Error(), "runs as root") {
		t.Fatalf("%v", err)
	}
	head := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	id := b.push(b.repo.bundleFiles(head, b.base))
	if err := runApply(nil, 0, b.applier(hooks{})); err != nil { //nolint:staticcheck // the nil context cobra can hand over
		t.Fatal(err)
	}
	if r := b.result(id); r == nil || r.Phase != phaseApplied {
		t.Fatalf("%+v", r)
	}
}
