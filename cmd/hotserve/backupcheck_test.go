package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/liveswap"
)

// Caddyfiles adapted by Caddy's own adapter, as validate and reload
// adapt them: where each backup declaration comes from is Caddy's word,
// after its imports, snippets and heredocs.

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // not cut by the umask
		t.Fatal(err)
	}
}

const app = "app blog {\n\tcommand /bin/true\n\tbackup {\n\t\tfiles .\n\t}\n}\n"

func global(body string) string { return "{\n\tliveswap {\n" + body + "\t}\n}\n" }

// config is a directory standing for /etc/hotserve, open to others as
// it is on a box, and one outside it.
func dirs(t *testing.T) (config, outside string) {
	t.Helper()
	config, outside = t.TempDir(), t.TempDir()
	for _, d := range []string{config, outside} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return config, outside
}

// checked asserts that a Caddyfile was looked at and passed — not
// skipped: a check that never ran passes as well as one that did.
func checked(t *testing.T, cmd string, flags ...string) {
	t.Helper()
	liveswap.ClearBackupSources()
	if err := checkBackups(cmd, flags); err != nil {
		t.Fatalf("%s %q: %v", cmd, flags, err)
	}
	if len(liveswap.BackupSources()) == 0 {
		t.Fatalf("%s %q: nothing was checked", cmd, flags)
	}
}

func TestABackupDeclaredWhereARunSeesItPasses(t *testing.T) {
	config, _ := dirs(t)
	write(t, config+"/apps/blog.caddy", app, 0o644)
	write(t, config+"/Caddyfile", global("\t\troot /var/lib/liveswap\n\t\timport apps/*.caddy\n"), 0o644)
	checked(t, "validate", "--config", config+"/Caddyfile")
	// A link that stays under the directory is followed there.
	if err := os.Symlink("apps", config+"/sites"); err != nil {
		t.Fatal(err)
	}
	write(t, config+"/Caddyfile.linked", global("\t\timport sites/*.caddy\n"), 0o644)
	checked(t, "validate", "--config", config+"/Caddyfile.linked")
	checked(t, "validate", "-c"+config+"/Caddyfile")
	checked(t, "reload", "-fc", config+"/Caddyfile")
}

// Every way Caddy takes the flag, the Caddyfile given relatively, and
// its adjacent Caddyfile with none. In a process of its own, started in
// the directory: Caddy resolves a relative Caddyfile against the working
// directory it first saw, and caches it (caddy.FastAbs), so a test that
// changed directory part way would be asking about another one.
func TestARelativeCaddyfileIsCheckedWhereCaddyReadsIt(t *testing.T) {
	if os.Getenv("BACKUPCHECK_RELATIVE") == "" {
		config, _ := dirs(t)
		write(t, config+"/apps/blog.caddy", app, 0o644)
		write(t, config+"/Caddyfile", global("\t\timport apps/*.caddy\n"), 0o644)
		child := exec.Command(os.Args[0], "-test.run=^TestARelativeCaddyfileIsCheckedWhereCaddyReadsIt$", "-test.v")
		child.Dir, child.Env = config, append(os.Environ(), "BACKUPCHECK_RELATIVE=1")
		if out, err := child.CombinedOutput(); err != nil || !strings.Contains(string(out), "--- PASS") {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	checked(t, "reload", "--config=Caddyfile", "--force")
	checked(t, "validate", "-c", "Caddyfile")
	checked(t, "validate", "-cCaddyfile")
	checked(t, "run")
}

func TestABackupDeclaredOutsideTheCaddyfilesDirectoryIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		caddyfile, outsideFile, outsideBody string
		want                                []string
	}{
		"an imported file": {
			global("\t\timport OUTSIDE/shop.caddy\n"), "shop.caddy", app,
			[]string{"blog's backup is read from OUTSIDE/shop.caddy, outside CONFIG"},
		},
		"a snippet defined outside": {
			"import OUTSIDE/snip.caddy\n" + global("\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t\timport bk\n\t\t}\n"),
			"snip.caddy", "(bk) {\n\tbackup {\n\t\tfiles .\n\t}\n}\n",
			[]string{"blog's backup is read from OUTSIDE/snip.caddy"},
		},
		"the whole global block, root and all": {
			"import OUTSIDE/global.caddy\n", "global.caddy", global("\t\troot /var/lib/liveswap\n" + app),
			[]string{"the liveswap root is read from OUTSIDE/global.caddy", "blog's backup is read from OUTSIDE/global.caddy"},
		},
		"an import by a heredoc": {
			global("\t\timport <<P\n\t\tOUTSIDE/shop.caddy\n\t\tP\n"), "shop.caddy", app,
			[]string{"blog's backup is read from OUTSIDE/shop.caddy"},
		},
		"only its lines, from an import inside the block": {
			global("\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t\tbackup {\n\t\t\t\timport OUTSIDE/lines.caddy\n\t\t\t}\n\t\t}\n"),
			"lines.caddy", "files .\n",
			[]string{"blog's backup is read from OUTSIDE/lines.caddy"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			config, outside := dirs(t)
			write(t, filepath.Join(outside, tc.outsideFile), tc.outsideBody, 0o644)
			write(t, config+"/Caddyfile", strings.ReplaceAll(tc.caddyfile, "OUTSIDE", outside), 0o644)
			err := checkBackups("validate", []string{"--config", config + "/Caddyfile"})
			for _, w := range tc.want {
				w = strings.NewReplacer("OUTSIDE", outside, "CONFIG", config).Replace(w)
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Fatalf("want %q in the error, got %v", w, err)
				}
			}
		})
	}
}

func TestWhatOthersMayNotReadIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		caddyfileMode, appsMode, fileMode os.FileMode
		want                              string
	}{
		"the Caddyfile":     {0o640, 0o755, 0o644, "the Caddyfile, CONFIG/Caddyfile, may not be read by others"},
		"the imported file": {0o644, 0o755, 0o640, "blog's backup is read from CONFIG/apps/blog.caddy, which others may not read"},
		"a directory":       {0o644, 0o750, 0o644, "blog's backup is read from under CONFIG/apps, which others may not enter"},
	} {
		t.Run(name, func(t *testing.T) {
			config, _ := dirs(t)
			write(t, config+"/apps/blog.caddy", app, tc.fileMode)
			write(t, config+"/Caddyfile", global("\t\timport apps/*.caddy\n"), tc.caddyfileMode)
			if err := os.Chmod(config+"/apps", tc.appsMode); err != nil {
				t.Fatal(err)
			}
			err := checkBackups("validate", []string{"--config", config + "/Caddyfile"})
			if w := strings.ReplaceAll(tc.want, "CONFIG", config); err == nil || !strings.Contains(err.Error(), w) {
				t.Fatalf("want %q in the error, got %v", w, err)
			}
		})
	}
}

// Nothing is asked of a Caddyfile that declares no backup, of one Caddy
// cannot adapt (its own command says why), or of a config that is not a
// Caddyfile.
func TestNothingIsAskedOfWhatHasNoBackupToLose(t *testing.T) {
	config, outside := dirs(t)
	write(t, outside+"/site.caddy", "http://:8080 {\n\trespond hi\n}\n", 0o600)
	write(t, config+"/Caddyfile", "import "+outside+"/site.caddy\n", 0o600)
	write(t, config+"/broken", global("\t\tnonsense\n")+app, 0o600)
	write(t, config+"/c.json", `{}`, 0o600)
	write(t, outside+"/shop.caddy", app, 0o644)
	write(t, config+"/sites.conf", global("\t\timport "+outside+"/shop.caddy\n"), 0o644)
	for _, flags := range [][]string{
		{"--config", config + "/Caddyfile"},
		{"--config", config + "/broken", "--adapter", "caddyfile"},
		{"--config", config + "/c.json"},
		{"--config", config + "/Caddyfile", "--adapter", "json"},
		// Caddy reads a name that neither begins "Caddyfile" nor ends
		// ".caddyfile" as JSON, with no --adapter; so is it left here.
		{"--config", config + "/sites.conf"},
	} {
		if err := checkBackups("validate", flags); err != nil {
			t.Errorf("%q: %v", flags, err)
		}
	}
}

// validate and reload stop; run serves, and says why.
func TestOnlyValidateAndReloadAreStopped(t *testing.T) {
	config, outside := dirs(t)
	write(t, outside+"/shop.caddy", app, 0o644)
	write(t, config+"/Caddyfile", global("\t\timport "+outside+"/shop.caddy\n"), 0o644)
	for cmd, stops := range map[string]bool{"validate": true, "reload": true, "run": false, "adapt": false, "version": false} {
		var stderr bytes.Buffer
		if got := gate([]string{cmd, "--config", config + "/Caddyfile"}, &stderr); got != stops {
			t.Errorf("%s: stop = %v, want %v", cmd, got, stops)
		}
		said := stderr.String()
		switch {
		case cmd == "run" && !strings.Contains(said, "served all the same"):
			t.Errorf("run said %q", said)
		case stops && !strings.HasPrefix(said, "Error: a backup run would not see"):
			t.Errorf("%s said %q", cmd, said)
		case (cmd == "adapt" || cmd == "version") && said != "":
			t.Errorf("%s said %q", cmd, said)
		}
	}
}

// A config that is not a regular file — stdin, a pipe — is Caddy's to
// read, once: read here first, Caddy would be given nothing, and a pipe
// with no writer would never answer.
func TestAConfigThatIsNotAFileIsLeftToCaddy(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "Caddyfile")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- checkBackups("validate", []string{"--config", fifo}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check opened a pipe nobody writes to")
	}
}

// A link under the directory that leads out of it: Caddy reads through
// it, and joins imports as written, so what it reads looks as if it were
// under the directory; in a backup run's view the link leads nowhere.
func TestABackupReadThroughALinkOutIsRefused(t *testing.T) {
	config, outside := dirs(t)
	write(t, outside+"/blog.caddy", app, 0o644)
	if err := os.Symlink(outside, config+"/apps"); err != nil {
		t.Fatal(err)
	}
	write(t, config+"/Caddyfile", global("\t\timport apps/*.caddy\n"), 0o644)
	err := checkBackups("validate", []string{"--config", config + "/Caddyfile"})
	if w := "blog's backup is read from " + config + "/apps/blog.caddy, which leads to " + outside + "/blog.caddy, outside " + config; err == nil || !strings.Contains(err.Error(), w) {
		t.Fatalf("want %q in the error, got %v", w, err)
	}
}

// The root of the filesystem is a directory like any other.
func TestUnderTheRoot(t *testing.T) {
	for _, tc := range []struct {
		p, dir string
		want   bool
	}{
		{"/etc/Caddyfile", "/", true},
		{"/etc/hotserve/a", "/etc/hotserve", true},
		{"/etc/hotserve-old/a", "/etc/hotserve", false},
		{"/etc/hotserve", "/etc/hotserve", false},
	} {
		if got := under(tc.p, tc.dir); got != tc.want {
			t.Errorf("under(%q, %q) = %v", tc.p, tc.dir, got)
		}
	}
}

// The check takes each command's flags from Caddy's own definition of
// it, not a copy: were that to stop being there, the check would skip
// itself, silently, for every Caddyfile.
func TestCaddysOwnFlagsAreWhereTheCheckTakesThem(t *testing.T) {
	for _, name := range []string{"validate", "reload", "run"} {
		fs, ok := commandFlags(name, nil)
		if !ok {
			t.Errorf("%s: no flags from Caddy's own command", name)
			continue
		}
		for _, flag := range []string{"config", "adapter"} {
			if fs.Lookup(flag) == nil {
				t.Errorf("%s: Caddy's command has no --%s", name, flag)
			}
		}
	}
}
