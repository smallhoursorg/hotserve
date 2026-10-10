package box

import (
	"slices"
	"strings"
	"testing"
)

// The happy push: the worked trace's steps 3 and 5 (DESIGN-box.md, "A
// worked trace"), every durable write in on-disk order.
func TestApplyHappy(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	c1 := b.repo.commit(v2, &b.alice, b.base)
	id := b.push(b.repo.bundleFiles(c1, b.base))
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if got := b.installed(); string(got) != string(v2) {
		t.Fatalf("installed file not the pushed one:\n%s", got)
	}
	if a := b.applied(); a.SHA != c1 || a.SHA256 != digest(v2) || a.Signer != "alice@example.com" || a.Path != testPath {
		t.Errorf("applied.json %+v", a)
	}
	r := b.result(id)
	if r == nil || r.Phase != phaseApplied || r.Error != "" || r.Commit != c1 || r.Signer != "alice@example.com" || r.BoxWebhook != "deploy.example.com" || r.OutOfBand {
		t.Fatalf("result %+v", r)
	}
	if !slices.Equal(r.Apps, []string{"example"}) || !strings.Contains(r.Diff, "-# version 1\n+# version 2\n") {
		t.Errorf("apps %v, diff:\n%s", r.Apps, r.Diff)
	}
	if b.sd.reloaded != 1 {
		t.Errorf("%d reloads", b.sd.reloaded)
	}
	want := []string{"take", "result:verified", "record:installing", "caddyfile:new", "record:swapped", "record:applied", "applied.json", "result:applied", "record:remove", "entry:remove"}
	if !slices.Equal(b.lastPoint, want) {
		t.Errorf("durable writes\n got %v\nwant %v", b.lastPoint, want)
	}
}

// A replay of the applied file in a new commit: no_change advances the
// baseline and writes nothing to /etc/hotserve.
func TestApplyNoChange(t *testing.T) {
	b := newTestBox(t)
	c1 := b.repo.commit(b.v1, &b.alice, b.base)
	id := b.push(b.repo.bundleFiles(c1, b.base))
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(id); r == nil || r.Phase != phaseNoChange {
		t.Fatalf("result %+v", r)
	}
	if a := b.applied(); a.SHA != c1 {
		t.Errorf("baseline %s, want %s", a.SHA, c1)
	}
	if b.sd.reloaded != 0 {
		t.Errorf("%d reloads", b.sd.reloaded)
	}
	want := []string{"take", "record:no_change", "applied.json", "result:no_change", "record:remove", "entry:remove"}
	if !slices.Equal(b.lastPoint, want) {
		t.Errorf("durable writes\n got %v\nwant %v", b.lastPoint, want)
	}
}

func TestApplyRefusedUnsigned(t *testing.T) {
	b := newTestBox(t)
	c1 := b.repo.commit(boxFile(2, b.alice), nil, b.base)
	id := b.push(b.repo.bundleFiles(c1, b.base))
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	r := b.result(id)
	if r == nil || r.Phase != phaseRefused || r.Error != c1+" is not signed; the box applies only commits signed by a key in its signer list" {
		t.Fatalf("result %+v", r)
	}
	if string(b.installed()) != string(b.v1) || b.applied().SHA != b.base {
		t.Error("a refusal changed something")
	}
	want := []string{"take", "result:refused", "entry:remove"}
	if !slices.Equal(b.lastPoint, want) {
		t.Errorf("durable writes\n got %v\nwant %v", b.lastPoint, want)
	}
}
