package box

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initDir is the box's directory as the operator copies it over: file
// as its Caddyfile, in a directory of its own.
func initDir(t *testing.T, file []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), file, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// initCmd runs `hotserve init <sha>` as root against the box, from dir.
func (b *testBox) initCmd(dir, sha string) (string, error) {
	b.t.Helper()
	var out bytes.Buffer
	err := runInitCmd(context.Background(), 0, b.applier(hooks{}), dir, sha, &out)
	return out.String(), err
}

const fromHere = "init: reloaded. From here, this box takes config only as signed commits sent by your repository's workflow.\n"

// summary is init's first two lines for a boxFile on host.
func summary(host string) string {
	return "init: box_webhook on " + host + "; 1 app (example); 1 signer (alice@example.com)\n" +
		"init: this box's file is " + host + "/Caddyfile in the repository\n"
}

// What `hotserve init` prints for each way a run can end, and what it
// leaves on disk (DESIGN-box.md, "`hotserve init`").
func TestInitCmd(t *testing.T) {
	onHost := func(file []byte, host string) []byte {
		return bytes.Replace(file, []byte("deploy.example.com {"), []byte(host+" {"), 1)
	}
	type check struct {
		b    *testBox
		file []byte
		sha  string
	}
	for _, c := range []struct {
		name string
		// setup prepares the box and returns the directory's file and
		// the commit init is given.
		setup func(b *testBox) ([]byte, string)
		want  func(c check) string
		err   string
		// path and digest are applied.json's afterwards: "" for the
		// baseline as newTestBox wrote it.
		path   string
		reload int
	}{
		{
			name: "a fresh box",
			setup: func(b *testBox) ([]byte, string) {
				b.writeInstalled([]byte("example.com {\n\trespond \"hello\"\n}\n")) // the packaged file walks as no box's
				if err := os.Remove(b.x("applied.json")); err != nil {
					t.Fatal(err)
				}
				file := boxFile(2, b.alice)
				return file, b.repo.commit(file, &b.alice)
			},
			want: func(c check) string {
				return summary("deploy.example.com") + "init: installed /etc/hotserve/Caddyfile\n" +
					"init: this box applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "deploy.example.com/Caddyfile", reload: 1,
		},
		{
			name: "a re-init over an earlier baseline",
			setup: func(b *testBox) ([]byte, string) {
				file := boxFile(2, b.alice)
				return file, b.repo.commit(file, &b.alice)
			},
			want: func(c check) string {
				return summary("deploy.example.com") + "init: installed /etc/hotserve/Caddyfile\n" +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "deploy.example.com/Caddyfile", reload: 1,
		},
		{
			name: "a re-init naming another host: warned, not refused",
			setup: func(b *testBox) ([]byte, string) {
				file := onHost(boxFile(2, b.alice), "other.example.com")
				return file, b.repo.commit(file, &b.alice)
			},
			want: func(c check) string {
				return summary("other.example.com") + "init: this box was deploy.example.com; this file names other.example.com\n" +
					"init: installed /etc/hotserve/Caddyfile\n" +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "other.example.com/Caddyfile", reload: 1,
		},
		{
			name: "a mixed-case host is recorded in lower case",
			setup: func(b *testBox) ([]byte, string) {
				file := onHost(boxFile(2, b.alice), "Deploy.Example.COM")
				return file, b.repo.commit(file, &b.alice)
			},
			want: func(c check) string {
				return summary("deploy.example.com") + "init: installed /etc/hotserve/Caddyfile\n" +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "deploy.example.com/Caddyfile", reload: 1,
		},
		{
			name:  "no_change at the baseline",
			setup: func(b *testBox) ([]byte, string) { return b.v1, b.base },
			want: func(c check) string {
				return summary("deploy.example.com") + "init: unchanged; the baseline is now " + c.sha[:7] + "\n"
			},
			path: "deploy.example.com/Caddyfile",
		},
		{
			name: "no_change over another baseline",
			setup: func(b *testBox) ([]byte, string) {
				return b.v1, b.repo.commit(b.v1, &b.alice, b.base)
			},
			want: func(c check) string {
				return summary("deploy.example.com") +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" +
					"init: unchanged; the baseline is now " + c.sha[:7] + "\n"
			},
			path: "deploy.example.com/Caddyfile",
		},
		{
			name: "a reload that fails: rolled back, non-zero",
			setup: func(b *testBox) ([]byte, string) {
				b.sd.reloads = []error{errReload}
				file := boxFile(2, b.alice)
				return file, b.repo.commit(file, &b.alice, b.base)
			},
			want:   func(check) string { return summary("deploy.example.com") },
			err:    phaseRolledBack + ": " + msgReloadFailed,
			reload: 2,
		},
		{
			name: "activating, then active within the wait: applied",
			setup: func(b *testBox) ([]byte, string) {
				b.sd.states = []string{"activating", "activating", "active"}
				file := boxFile(2, b.alice)
				return file, b.repo.commit(file, &b.alice, b.base)
			},
			want: func(c check) string {
				return summary("deploy.example.com") + "init: installed /etc/hotserve/Caddyfile\n" +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "deploy.example.com/Caddyfile", reload: 1,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBox(t)
			was := b.applied()
			file, sha := c.setup(b)
			out, err := b.initCmd(initDir(t, file), sha)
			switch {
			case c.err == "" && err != nil:
				t.Fatal(err)
			case c.err != "" && (err == nil || err.Error() != c.err):
				t.Fatalf("%v, want %q", err, c.err)
			}
			if want := c.want(check{b: b, file: file, sha: sha}); out != want {
				t.Errorf("printed\n%s\nwant\n%s", out, want)
			}
			b.settled()
			if n := b.names(b.x("out")); len(n) != 0 {
				t.Errorf("init wrote results: %v", n)
			}
			if b.sd.reloaded != c.reload {
				t.Errorf("%d reloads, want %d", b.sd.reloaded, c.reload)
			}
			a := b.applied()
			if c.path == "" {
				if a != was || !bytes.Equal(b.installed(), b.v1) {
					t.Errorf("a red init changed the box: %+v", a)
				}
				return
			}
			if a.SHA != sha || a.Path != c.path || a.SHA256 != digest(file) || a.Signer != "init" || !bytes.Equal(b.installed(), file) {
				t.Errorf("applied.json %+v", a)
			}
		})
	}
}

// init takes root's lock and settles a record a crash left before its
// own transaction, as the applier would have.
func TestInitCmdRecoversFirst(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	b.writeInstalled(v2)
	crashed := randomID(t)
	b.lyingRecord(record{ID: crashed, Origin: originApplier, Phase: phaseSwapped, Commit: b.repo.commit(v2, &b.alice, b.base), Path: testPath,
		Signer: "alice@example.com", Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
	v3 := boxFile(3, b.alice)
	sha := b.repo.commit(v3, &b.alice, b.base)
	if _, err := b.initCmd(initDir(t, v3), sha); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(crashed); r == nil || r.Phase != phaseRolledBack {
		t.Errorf("the crashed push: %+v", r)
	}
	if !bytes.Equal(b.installed(), v3) || b.applied().SHA != sha {
		t.Error("init did not apply after recovery")
	}
}

// Every refusal is by name, and writes nothing: refusals before the lock
// print nothing either; a hotserve that is not up refuses after the
// summary, with the remedy filled in (DESIGN-box.md, "Console
// commands").
func TestInitCmdRefusals(t *testing.T) {
	edit := func(old, new string) func(*testBox) []byte {
		return func(*testBox) []byte {
			if !strings.Contains(good, old) {
				t.Fatalf("the fixture has no %q", old)
			}
			return file(strings.Replace(good, old, new, 1))
		}
	}
	whole := func(b *testBox) []byte { return b.v1 }
	big := func(*testBox) []byte { return bytes.Repeat([]byte("#"), 1<<20+1) }
	const signers = "\t\tsigner alice@example.com ssh-ed25519 K1\n\t\tsigner bob ssh-ed25519 K2\n"
	const trust = "\t\tdeploy_trust github {\n\t\t\taudience hotserve\n\t\t\tclaim repository your-org/boxes\n\t\t}\n"
	const boxBlock = "\tbox {\n" + trust + "\t\t# signers\n" + signers + "\t}\n"
	for _, c := range []struct {
		name   string
		euid   int
		file   func(b *testBox) []byte // nil: no Caddyfile in the directory
		dir    func(t *testing.T, b *testBox) string
		sha    string // "": a valid commit
		states []string
		want   string
		remedy bool
	}{
		{name: "not root", euid: 1000, file: whole, want: "hotserve init runs as root: it installs /etc/hotserve/Caddyfile"},
		{name: "a short sha", file: whole, sha: "4f1c2a9", want: "4f1c2a9 is not a commit id: 40 lowercase hex, as git rev-parse HEAD prints one"},
		{name: "an upper-case sha", file: whole, sha: strings.Repeat("A", 40), want: strings.Repeat("A", 40) + " is not a commit id"},
		{name: "a sha that is not hex", file: whole, sha: strings.Repeat("z", 40), want: "is not a commit id"},
		{name: "no Caddyfile here", want: "no Caddyfile here; run init from inside the box's directory"},
		{name: "a Caddyfile over the cap", file: big, want: "Caddyfile: larger than 1048576 bytes"},
		{name: "a directory named Caddyfile", dir: func(t *testing.T, _ *testBox) string {
			d := t.TempDir()
			if err := os.Mkdir(filepath.Join(d, "Caddyfile"), 0o755); err != nil {
				t.Fatal(err)
			}
			return d
		}, want: "Caddyfile: not a regular file"},
		{name: "a FIFO named Caddyfile", dir: func(t *testing.T, _ *testBox) string {
			d := t.TempDir()
			mkfifo(t, filepath.Join(d, "Caddyfile"))
			return d
		}, want: "Caddyfile: not a regular file"},
		{name: "no box block", file: edit(boxBlock, ""), want: "the Caddyfile has no box block"},
		{name: "no signer", file: edit(signers, ""), want: "the Caddyfile has no signer"},
		{name: "no deploy_trust", file: edit(trust, ""), want: "the Caddyfile has no deploy_trust"},
		{name: "no box_webhook", file: edit("\tbox_webhook\n", ""), want: "the Caddyfile has no site with box_webhook"},
		{name: "two box_webhook sites", file: edit("deploy.example.com {", "other.example.com {\n\tbox_webhook\n}\n\ndeploy.example.com {"), want: "the Caddyfile has more than one site with box_webhook"},
		{name: "a wildcard host", file: edit("deploy.example.com {", "*.example.com {"), want: "the Caddyfile has a box_webhook site whose address is not one bare hostname (*.example.com)"},
		{name: "a placeholder host", file: edit("deploy.example.com {", "{$HOST} {"), want: "the Caddyfile has a placeholder where a directive name, a site address or a box line goes ({$HOST})"},
		{name: "no host", file: edit("deploy.example.com {", ":443 {"), want: "the Caddyfile has a box_webhook site whose address is not one bare hostname (:443)"},
		{name: "an import in the box block", file: edit("\t\t# signers\n", "\t\timport signers\n"), want: "the Caddyfile imports signers; inline the snippet"},
		{name: "an import in the site", file: edit("\tbox_webhook\n", "\tbox_webhook\n\timport extra\n"), want: "the Caddyfile imports extra; inline the snippet"},
		{name: "hotserve inactive", file: whole, states: []string{"inactive"}, want: msgNotRunning, remedy: true},
		{name: "hotserve failed", file: whole, states: []string{"failed"}, want: msgNotRunning, remedy: true},
		{name: "hotserve still activating after the wait", file: whole, states: []string{"activating"}, want: msgStillStarting, remedy: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBox(t)
			b.sd.states = c.states
			sha := c.sha
			if sha == "" {
				sha = b.repo.commit(b.v1, &b.alice, b.base)
			}
			var dir string
			switch {
			case c.dir != nil:
				dir = c.dir(t, b)
			case c.file != nil:
				dir = initDir(t, c.file(b))
			default:
				dir = t.TempDir()
			}
			snap := b.snapshot()
			var out bytes.Buffer
			err := runInitCmd(context.Background(), c.euid, b.applier(hooks{}), dir, sha, &out)
			want := c.want
			if c.remedy {
				want += "\n" + fmt.Sprintf(msgInitRemedy, sha)
				if err == nil || err.Error() != want {
					t.Fatalf("%v\nwant\n%s", err, want)
				}
				if s := out.String(); s != summary("deploy.example.com") {
					t.Errorf("printed %q", s)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%v, want %q", err, want)
				}
				if out.Len() != 0 {
					t.Errorf("printed %q before a refusal", out.String())
				}
			}
			if !maps.Equal(snap, b.snapshot()) {
				t.Error("a refused init changed the box")
			}
			if b.sd.reloaded != 0 {
				t.Error("a refused init reloaded")
			}
		})
	}
}
