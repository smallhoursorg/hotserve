package box

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// The worst result the caps allow fits the handler's read cap with
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

// A result at every cap with secrets — 8192 names removed and 8192 added
// or changed, each the longest `<app>/<VAR>` the grammars allow, beside
// the apps and diff above — fits the handler's 6 MiB and reads back;
// past the caps the verdict survives and the names give way last.
func TestResultWithSecretsFitsTheHandlersCap(t *testing.T) {
	apps := make([]string, (1<<20)/6)
	for i := range apps {
		apps[i] = "a"
	}
	name := func(i int) string {
		return strings.Repeat("a", 63) + "/" + fmt.Sprintf("V%0*d", proof.MaxVarName-1, i)
	}
	names := func(from, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = name(from + i)
		}
		return out
	}
	r := result{
		ID: strings.Repeat("a", 32), Phase: phaseRolledBack, Commit: strings.Repeat("c", 40),
		Path: strings.Repeat("\xff", 4096), Signer: strings.Repeat("s", 256), BoxWebhook: strings.Repeat("h", 253),
		Apps: apps, Diff: strings.Repeat("\x01", maxDiff), Error: strings.Repeat("\"", 4096),
		SecretsRemoved: names(0, maxSecretNames), SecretsAdded: names(maxSecretNames, maxSecretNames/2), SecretsChanged: names(3*maxSecretNames/2, maxSecretNames/2),
	}
	b := r.marshal()
	if len(b) > maxResult || bytes.Contains(b, []byte("omitted")) {
		t.Fatalf("%d bytes, the extras omitted: %v", len(b), bytes.Contains(b, []byte("omitted")))
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out", r.ID+".json"), b, 0o640); err != nil {
		t.Fatal(err)
	}
	if got, err := readResultFile(dir, r.ID); err != nil || len(got.SecretsRemoved) != maxSecretNames {
		t.Fatalf("does not read back: %v", err)
	}
	// Past the caps: the diff and apps give way first, then the names.
	r.Apps = slices.Concat(apps, apps, apps, apps, apps, apps, apps, apps)
	r.SecretsChanged = names(0, 3*maxSecretNames)
	if b := r.marshal(); len(b) > maxResult || !bytes.Contains(b, []byte(`"phase":"rolled_back"`)) || bytes.Contains(b, []byte(`"secrets_changed"`)) {
		t.Errorf("%d bytes past the caps", len(b))
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
	for _, k := range []string{"id", "phase", "commit", "path", "signer", "box", "caddyfile_edited_out_of_band", "apps", "diff", "error", "named", "write_error"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("the line has no %s", k)
		}
	}
	if fields["id"] != id || fields["phase"] != phaseApplied || fields["commit"] != head {
		t.Errorf("%v", fields)
	}

	// A refusal's named ids reach the line too.
	b = newTestBox(t)
	unsigned := b.repo.commit(boxFile(2, b.alice), nil, b.base)
	b.push(b.repo.bundleFiles(unsigned, b.base))
	if err := b.run(hooks{fail: failAt(map[string]int{"result:refused": -1})}); err != nil {
		t.Fatal(err)
	}
	lines = b.logged(zapcore.ErrorLevel, "box result could not be written")
	if len(lines) != 1 {
		t.Fatalf("refused: %d lines", len(lines))
	}
	if named, _ := lines[0].ContextMap()["named"].([]any); len(named) != 1 || named[0] != unsigned {
		t.Errorf("refused: named = %v", lines[0].ContextMap()["named"])
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
