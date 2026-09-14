package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A record is written for every deploy — a failure with its detail, a
// success — as the filter left it, and read back whole; the status
// lists each version's outcome newest first.
func TestDeployRecordsAreWrittenAndListed(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("SECRET=hunter2hunter2hunter2\n"), 0o600))
	rig.runner.runOnceErr = runOnceExit("exit status 3", "u", "failed")
	rig.spec.preStart = []string{"./migrate"}
	rig.spec.deployLogLines = 40
	rig.ma.journal = &fakeJournal{lines: []string{"migrate: SECRET=hunter2hunter2hunter2 refused"}}
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("v1 should have failed in pre_start")
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	if err != nil {
		t.Fatalf("no record for the failed v1: %v", err)
	}
	var got deployResult
	must(t, json.Unmarshal(rec, &got))
	if got.Version != "v1" || got.Status != "failed" || got.Phase != "preparing" || got.Detail == nil || got.Detail.Exit != "exit status 3" {
		t.Fatalf("record = %+v", got)
	}
	if s := string(rec); strings.Contains(s, "hunter2") || !strings.Contains(s, "[redacted:SECRET]") || !strings.Contains(s, `"redacted_env":["SECRET"]`) {
		t.Fatalf("the record must be the filtered result: %s", s)
	}

	rig.runner.runOnceErr = nil
	rig.clock.Advance(time.Minute)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v2.tgz", version: "v2", by: "test"}); err != nil {
		t.Fatal(err)
	}
	st := rig.ma.status()
	if len(st.Deploys) != 2 || st.Deploys[0].Version != "v2" || st.Deploys[0].Status != "succeeded" || st.Deploys[1].Version != "v1" || st.Deploys[1].Status != "failed" || st.Deploys[1].Phase != "preparing" {
		t.Fatalf("status.deploys = %+v", st.Deploys)
	}
	if _, err := readDeployRecord(rig.spec.dirs, "v9"); !errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a version never deployed: %v", err)
	}
}

// Records are pruned with the releases: every version still on disk
// keeps its record, and the newest keep others survive.
func TestDeployRecordsArePrunedWithReleases(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.keep = 2
	fail := func(v string) {
		rig.runner.startErr = errors.New("boom")
		rig.clock.Advance(time.Minute)
		if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/" + v, version: v, by: "test"}); err == nil {
			t.Fatalf("%s should have failed", v)
		}
		rig.runner.startErr = nil
	}
	ok := func(v string) {
		rig.clock.Advance(time.Minute)
		if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/" + v, version: v, by: "test"}); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
	}
	ok("v1")
	fail("f1")
	fail("f2")
	fail("f3")
	ok("v2")
	ok("v3") // release GC keeps 2: v3, v2 (v1 pruned)
	var names []string
	for _, d := range rig.ma.status().Deploys {
		names = append(names, d.Version)
	}
	// On disk: v3, v2 → kept. Others by the time their record was
	// written: f3, f2, f1, v1 → the newest 2 (f3, f2) kept; v1's
	// record went with its release.
	if got := strings.Join(names, ","); got != "v3,v2,f3,f2" {
		t.Fatalf("records after pruning = %s", got)
	}
}

func TestPruneDeployRecordsKeepsOnDiskAndNewest(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	dir := d.deploys
	base := time.Now().Add(-time.Hour)
	for i, v := range []string{"a", "b", "c", "d"} {
		must(t, writeDeployRecord(d, v, []byte(`{"version":"`+v+`"}`)))
		must(t, os.Chtimes(deployRecordPath(dir, v), base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute)))
	}
	must(t, os.WriteFile(filepath.Join(dir, ".record-123.tmp"), []byte("{"), 0o600)) // a write that never got its rename
	// Strays hold no retention slot and are removed: a file that is
	// not JSON, a record whose version is not its name, a link.
	must(t, os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "wrong.json"), []byte(`{"version":"other"}`), 0o600))
	must(t, os.Symlink("a.json", filepath.Join(dir, "link.json")))
	pruneDeployRecords(dir, 1, []string{"a"}, zap.NewNop())
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if got := strings.Join(left, ","); got != "a.json,d.json" {
		t.Fatalf("left = %s", got)
	}
}

// The records live beside proxy/, outside every instance's view.
func TestDeployRecordsAreOutsideTheSandboxView(t *testing.T) {
	spec := testSpec(t)
	sb := spec.sandboxSpecFor(spec.dirs.release("v1"), recordedNonce)
	if sb.inView(spec.dirs.deploys) || sb.inView(deployRecordPath(spec.dirs.deploys, "v1")) {
		t.Fatal("the deploys dir must not be inside the sandbox view")
	}
}

// A filter that replaced a timestamp-shaped value leaves the record in
// the list: the times are carried raw, and the order is the write time.
func TestDeploySummariesSurviveARedactedTimestamp(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	must(t, writeDeployRecord(d, "v1", []byte(`{"version":"v1","status":"succeeded","started_at":"[redacted:STAMP]","finished_at":"[redacted:STAMP]"}`)))
	got := listDeploySummaries(d)
	if len(got) != 1 || got[0].Version != "v1" || string(got[0].FinishedAt) != `"[redacted:STAMP]"` {
		t.Fatalf("summaries = %+v", got)
	}
}

// A releases directory that cannot be listed is no reason to prune:
// every record stays, rather than an on-disk release losing its.
func TestDeployRecordsAreNotPrunedWhenReleasesAreUnreadable(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.keep = 1
	for _, v := range []string{"a", "b", "c"} {
		must(t, writeDeployRecord(rig.spec.dirs, v, []byte(`{"version":"`+v+`","status":"failed"}`)))
	}
	must(t, os.RemoveAll(rig.spec.dirs.releases))
	must(t, os.WriteFile(rig.spec.dirs.releases, []byte("not a dir"), 0o600)) // ReadDir fails
	rig.ma.recordDeploy(rig.ma.snapshot(), deployResult{Version: "d", Status: "failed"})
	if got := len(listDeploySummaries(rig.spec.dirs)); got != 4 {
		t.Fatalf("records after an unreadable releases dir = %d, want all 4 kept", got)
	}
}

// A refusal before any phase — re-posting the running version, or a
// version already on disk — writes no record: the record of the deploy
// that put the version there stays as it was.
func TestDeployRecordSurvivesARefusedRepost(t *testing.T) {
	rig := newTestRig(t)
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	before, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	err = deployOnceV1(t, rig)
	var ve validationError
	if !errors.As(err, &ve) {
		t.Fatalf("re-posting the running version should be refused: %v", err)
	}
	after, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	if string(after) != string(before) || !strings.Contains(string(after), `"status":"succeeded"`) {
		t.Fatalf("the refusal replaced v1's record:\n%s\n%s", before, after)
	}
	if d := rig.ma.status().Deploys; len(d) != 1 || d[0].Status != "succeeded" {
		t.Fatalf("deploys = %+v", d)
	}
}

// A failure before the first phase that is not a refusal — the box's
// own, here a releases path that is not a directory — is the version's
// history and is recorded like any other.
func TestDeployRecordKeepsAPrePhaseFailure(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	must(t, os.RemoveAll(rig.spec.dirs.releases))
	must(t, os.WriteFile(rig.spec.dirs.releases, []byte("not a dir"), 0o600))
	err := deployOnceV1(t, rig)
	var ve validationError
	if err == nil || errors.As(err, &ve) {
		t.Fatalf("want an operational failure, got %v", err)
	}
	rec, rerr := readDeployRecord(rig.spec.dirs, "v1")
	if rerr != nil || !strings.Contains(string(rec), `"status":"failed"`) {
		t.Fatalf("a pre-phase operational failure must be recorded: %v %s", rerr, rec)
	}
}

// A link planted where the records directory should be — by an app
// that could write its app dir before sandboxing existed — is not
// followed: nothing is written through it, read through it, or listed
// from it.
func TestDeployRecordsRefuseAPlantedDirectoryLink(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	must(t, os.Symlink("..", rig.spec.dirs.deploys)) // deploys -> the app dir: version "state" would be state.json
	rig.ma.recordDeploy(rig.ma.snapshot(), deployResult{Version: "state", Status: "failed"})
	if _, err := os.Lstat(filepath.Join(rig.spec.dirs.app, "state.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a record was written through the planted link: %v", err)
	}
	if _, err := readDeployRecord(rig.spec.dirs, "state"); err == nil || errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a read through the planted link must be refused outright, got %v", err)
	}
	if got := listDeploySummaries(rig.spec.dirs); got != nil {
		t.Fatalf("listed through the planted link: %+v", got)
	}
}

// A record name that is a link is neither served nor summarised, and a
// record whose version is not the name it sits under is not listed.
func TestDeployRecordsDoNotFollowARecordLinkOrTrustAForgedVersion(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	dir, err := deploysDir(d)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte(`{"version":"v1","status":"succeeded"}`), 0o600))
	must(t, os.Symlink("other.txt", deployRecordPath(dir, "v1")))
	if _, err := readDeployRecord(d, "v1"); err == nil || errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a linked record must be refused outright, got %v", err)
	}
	must(t, writeDeployRecord(d, "v2", []byte(`{"version":"AKIAFORGEDSECRETVALUE","status":"succeeded"}`)))
	// A version that is not a version, whose path component happens
	// to equal the name it sits under, is not one of ours either.
	must(t, os.WriteFile(filepath.Join(dir, "TOKEN.json"), []byte(`{"version":"../TOKEN","status":"succeeded"}`), 0o600))
	if got := listDeploySummaries(d); len(got) != 0 {
		t.Fatalf("a linked record or a forged version reached the list: %+v", got)
	}
}

// A planted temp name is never written through: the temp file is
// created fresh under a random name, and the leftover link is pruned.
func TestDeployRecordWriteNeverUsesAPlantedTempName(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	dir, err := deploysDir(d)
	must(t, err)
	target := filepath.Join(dir, "victim.json")
	must(t, os.WriteFile(target, []byte("precious"), 0o600))
	must(t, os.Symlink("victim.json", filepath.Join(dir, "v1.json.tmp")))
	must(t, writeDeployRecord(d, "v1", []byte(`{"version":"v1","status":"failed"}`)))
	if b, _ := os.ReadFile(target); string(b) != "precious" {
		t.Fatalf("the planted temp link's target was rewritten: %q", b)
	}
	pruneDeployRecords(dir, 5, nil, zap.NewNop())
	if _, err := os.Lstat(filepath.Join(dir, "v1.json.tmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the planted temp link was not pruned: %v", err)
	}
}

// A record under a version's name that is not that version's — stale,
// planted, corrupted, or not an object at all — is not served as it.
func TestDeployRecordMustNameItsVersion(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	must(t, writeDeployRecord(d, "v1", []byte(`{"version":"v2","status":"succeeded"}`)))
	must(t, writeDeployRecord(d, "v3", []byte(`["not","an","object"]`)))
	for _, v := range []string{"v1", "v3"} {
		if _, err := readDeployRecord(d, v); err == nil || errors.Is(err, errNoDeployRecord) {
			t.Fatalf("%s: a record that is not the version's must be refused outright, got %v", v, err)
		}
	}
	must(t, writeDeployRecord(d, "v4", []byte(`{"version":"v4","status":"failed"}`)))
	if _, err := readDeployRecord(d, "v4"); err != nil {
		t.Fatal(err)
	}
}

// The record is filtered with the deploy's own spec and the values
// known at the time, never withheld: a reload to an unreadable
// env_file while the deploy ran must not turn its outcome into a
// placeholder nothing can recover.
func TestDeployRecordIsNotWithheldByALaterUnreadableEnvFile(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("SECRET=hunter2hunter2hunter2\n"), 0o600))
	rig.ma.rememberSecrets(rig.spec.envFile, []string{"SECRET=hunter2hunter2hunter2"})
	c := rig.ma.snapshot() // the deploy's snapshot, env_file readable
	// The reload: the live spec now names an env_file that does not
	// exist, so the response filter would withhold every body.
	live := *rig.spec
	live.envFile = filepath.Join(t.TempDir(), "missing.env")
	rig.ma.spec = &live
	rig.ma.recordDeploy(c, deployResult{Version: "v1", Status: "failed", Error: "pre_start failed: SECRET=hunter2hunter2hunter2 refused"})
	rec, err := readDeployRecord(c.spec.dirs, "v1")
	if err != nil {
		t.Fatalf("the record was withheld or not written: %v", err)
	}
	if s := string(rec); !strings.Contains(s, `"status":"failed"`) || strings.Contains(s, "hunter2") || !strings.Contains(s, "[redacted:SECRET]") {
		t.Fatalf("record = %s", s)
	}
	if w := rig.ma.redactorFor(rig.ma.status()); w.withhold == "" {
		t.Fatal("the test's premise: the live filter withholds while the env_file is unreadable")
	}
}

// An ancestor that is a link — `<root>/blog -> <root>/shop`, planted
// by a pre-sandbox app — is not followed either: blog's records are
// not written, read or listed from shop's directory.
func TestDeployRecordsRefuseAPlantedAncestorLink(t *testing.T) {
	root := t.TempDir()
	shop := newAppDirs(root, "shop")
	must(t, os.MkdirAll(shop.app, 0o750))
	must(t, os.Symlink("shop", filepath.Join(root, "blog")))
	blog := newAppDirs(root, "blog")
	if _, err := deploysDir(blog); err == nil {
		t.Fatal("blog's deploys dir resolves into shop's and must be refused")
	}
	// Refused before anything was created through the link: shop has
	// no deploys dir that blog's request made for it.
	if _, err := os.Lstat(shop.deploys); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("blog's refused request created shop's deploys dir: %v", err)
	}
	must(t, writeDeployRecord(shop, "v1", []byte(`{"version":"v1","status":"succeeded"}`)))
	if err := writeDeployRecord(blog, "v9", []byte(`{"version":"v9","status":"failed"}`)); err == nil {
		t.Fatal("a write through the ancestor link must be refused")
	}
	if _, err := os.Lstat(deployRecordPath(shop.deploys, "v9")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("blog's record landed in shop's directory")
	}
	if _, err := readDeployRecord(blog, "v1"); err == nil || errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a read through the ancestor link must be refused outright, got %v", err)
	}
	if got := listDeploySummaries(blog); got != nil {
		t.Fatalf("shop's records listed as blog's: %+v", got)
	}
	// An alias on the root itself is the one difference allowed.
	alias := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(root, alias))
	if _, err := deploysDir(newAppDirs(alias, "shop")); err != nil {
		t.Fatalf("a symlinked root is a legitimate layout: %v", err)
	}
}

// A record planted under a name equal to a known env value must not
// let that value through the response filter: the status keeps the
// value redacted even though the same string is a "version".
func TestARecordedVersionEqualToAKnownValueIsStillRedacted(t *testing.T) {
	rig := newTestRig(t)
	secret := "q7Wm2xK9pL4vB8nR3tY6zH5c" // gitleaks:allow — a made-up value for this test
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("TOKEN="+secret+"\n"), 0o600))
	must(t, writeDeployRecord(rig.spec.dirs, secret, []byte(`{"version":"`+secret+`","status":"succeeded"}`)))
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	s := rig.ma.status()
	if len(s.Deploys) == 0 {
		t.Fatal("the planted record is listed (its version matches its name); the filter is what must catch it")
	}
	raw, err := json.Marshal(s)
	must(t, err)
	body := rig.ma.redactorFor(s).redactJSON(raw)
	if strings.Contains(body, secret) || !strings.Contains(body, "[redacted:TOKEN]") {
		t.Fatalf("the known value leaked through a recorded version: %s", body)
	}
}

// A release directory named after a known env value — plantable while
// the app dir was writable — does not exempt that value from the
// record's filter.
func TestRecordFilterDoesNotTrustAReleaseNameEqualToAKnownValue(t *testing.T) {
	rig := newTestRig(t)
	secret := "q7Wm2xK9pL4vB8nR3tY6zH5c" // gitleaks:allow — a made-up value for this test
	rig.ma.rememberSecrets("/etc/app.env", []string{"TOKEN=" + secret})
	must(t, os.MkdirAll(rig.spec.dirs.release(secret), 0o755))
	rec := rig.ma.recordRedactor(rig.ma.snapshot(), deployResult{Version: "v1"}).redactJSON([]byte(`{"version":"v1","error":"got ` + secret + `"}`))
	if strings.Contains(rec, secret) || !strings.Contains(rec, "[redacted:TOKEN]") {
		t.Fatalf("a planted release name exempted the value: %s", rec)
	}
}

// A deploy that failed before its launch remembered no env_file
// values, and its error can still carry one: the record's filter
// reads the deploy's own env_file, so the value is not written.
func TestDeployRecordFiltersACurrentValueBeforeAnyLaunch(t *testing.T) {
	rig := newTestRig(t)
	secret := "q7Wm2xK9pL4vB8nR3tY6zH5c" // gitleaks:allow — a made-up value for this test
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("TOKEN="+secret+"\n"), 0o600))
	rig.fetch.err = errors.New("download https://x/a.tgz?sig=" + secret + ": HTTP 403")
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("the download should have failed")
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	must(t, err)
	// The value is gone and the key reported; which layer took it (the
	// query-string rule reaches a URL's query before the marker does)
	// is not the point.
	if s := string(rec); strings.Contains(s, secret) || !strings.Contains(s, `"redacted_env":["TOKEN"]`) {
		t.Fatalf("a current value reached the record of a pre-launch failure: %s", s)
	}
}

// A known value that is a fragment of the record's own JSON makes the
// filter withhold the whole body; the record is then an envelope that
// still names the version and status, readable and listed.
func TestDeployRecordSurvivesTheFiltersWholeBodyFallback(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.rememberSecrets("/etc/app.env", []string{`WEIRD="status":"failed","error"`})
	rig.runner.startErr = errors.New("boom")
	if err := deployOnceV1(t, rig); err == nil {
		t.Fatal("v1 should have failed")
	}
	rec, err := readDeployRecord(rig.spec.dirs, "v1")
	if err != nil {
		t.Fatalf("the record must be readable as v1's: %v", err)
	}
	if s := string(rec); !strings.Contains(s, `"version":"v1"`) || !strings.Contains(s, `"status":"failed"`) || !strings.Contains(s, "record withheld") || strings.Contains(s, "boom") {
		t.Fatalf("envelope = %s", s)
	}
	if d := rig.ma.status().Deploys; len(d) != 1 || d[0].Version != "v1" || d[0].Status != "failed" {
		t.Fatalf("deploys = %+v", d)
	}
}
