package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestABackupDeclaredWhereARunSeesItPasses(t *testing.T) {
	config, _ := dirs(t)
	write(t, config+"/apps/blog.caddy", app, 0o644)
	write(t, config+"/Caddyfile", global("\t\troot /var/lib/liveswap\n\t\timport apps/*.caddy\n"), 0o644)
	if err := checkBackups([]string{"--config", config + "/Caddyfile"}); err != nil {
		t.Fatal(err)
	}
	// and with the flag written with =, and a Caddyfile given relatively
	t.Chdir(config)
	if err := checkBackups([]string{"--config=Caddyfile", "--force"}); err != nil {
		t.Fatalf("relative: %v", err)
	}
}

func TestABackupDeclaredOutsideTheCaddyfilesDirectoryIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		caddyfile, outsideFile, outsideBody string
		want                                []string
	}{
		"an imported file": {
			global("\t\timport OUTSIDE/shop.caddy\n"), "shop.caddy", app,
			[]string{"blog's backup is declared in OUTSIDE/shop.caddy, outside CONFIG"},
		},
		"a snippet defined outside": {
			"import OUTSIDE/snip.caddy\n" + global("\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t\timport bk\n\t\t}\n"),
			"snip.caddy", "(bk) {\n\tbackup {\n\t\tfiles .\n\t}\n}\n",
			[]string{"blog's backup is declared in OUTSIDE/snip.caddy"},
		},
		"the whole global block, root and all": {
			"import OUTSIDE/global.caddy\n", "global.caddy", global("\t\troot /var/lib/liveswap\n" + app),
			[]string{"the liveswap root is set in OUTSIDE/global.caddy", "blog's backup is declared in OUTSIDE/global.caddy"},
		},
		"an import by a heredoc": {
			global("\t\timport <<P\n\t\tOUTSIDE/shop.caddy\n\t\tP\n"), "shop.caddy", app,
			[]string{"blog's backup is declared in OUTSIDE/shop.caddy"},
		},
		"only its lines, from an import inside the block": {
			global("\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t\tbackup {\n\t\t\t\timport OUTSIDE/lines.caddy\n\t\t\t}\n\t\t}\n"),
			"lines.caddy", "files .\n",
			[]string{"blog's backup is declared in OUTSIDE/lines.caddy"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			config, outside := dirs(t)
			write(t, filepath.Join(outside, tc.outsideFile), tc.outsideBody, 0o644)
			write(t, config+"/Caddyfile", strings.ReplaceAll(tc.caddyfile, "OUTSIDE", outside), 0o644)
			err := checkBackups([]string{"--config", config + "/Caddyfile"})
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
		"the Caddyfile":     {0o640, 0o755, 0o644, "the Caddyfile is in CONFIG/Caddyfile, which others may not read"},
		"the imported file": {0o644, 0o755, 0o640, "blog's backup is declared in CONFIG/apps/blog.caddy, which others may not read"},
		"a directory":       {0o644, 0o750, 0o644, "blog's backup is declared under CONFIG/apps, which others may not enter"},
	} {
		t.Run(name, func(t *testing.T) {
			config, _ := dirs(t)
			write(t, config+"/apps/blog.caddy", app, tc.fileMode)
			write(t, config+"/Caddyfile", global("\t\timport apps/*.caddy\n"), tc.caddyfileMode)
			if err := os.Chmod(config+"/apps", tc.appsMode); err != nil {
				t.Fatal(err)
			}
			err := checkBackups([]string{"--config", config + "/Caddyfile"})
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
	for _, flags := range [][]string{
		{"--config", config + "/Caddyfile"},
		{"--config", config + "/broken", "--adapter", "caddyfile"},
		{"--config", config + "/c.json"},
		{"--config", config + "/Caddyfile", "--adapter", "json"},
	} {
		if err := checkBackups(flags); err != nil {
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
	go func() { done <- checkBackups([]string{"--config", fifo}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check opened a pipe nobody writes to")
	}
}
