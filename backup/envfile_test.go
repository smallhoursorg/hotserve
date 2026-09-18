package backup

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.env")
	body := "# written by hotserve backup init\n\nRESTIC_REPOSITORY=s3:example/bucket\nRESTIC_PASSWORD=secret=with=equals\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"RESTIC_REPOSITORY=s3:example/bucket", "RESTIC_PASSWORD=secret=with=equals"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %v, want %v", env, want)
	}
}

// systemd strips surrounding quotes from an EnvironmentFile= value
// before the jobs see it. Reading the same file any other way has to
// match, or status and restore read a different repository from the
// one the jobs use.
func TestLoadEnvFileStripsQuotesLikeSystemd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.env")
	body := "RESTIC_REPOSITORY=\"s3:host/bucket\"\nRESTIC_PASSWORD='p a s s'\nAWS_ACCESS_KEY_ID=plain\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"RESTIC_REPOSITORY=s3:host/bucket", "RESTIC_PASSWORD=p a s s", "AWS_ACCESS_KEY_ID=plain"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q, want %q", env, want)
	}
	if err := checkSettingsRepository(env); err != nil {
		t.Fatalf("a quoted backend URL must still be recognised as one: %v", err)
	}
}

// Running `hotserve backup status` on a box where backups were never
// set up should say that, not fail somewhere inside restic.
func TestLoadEnvFileMissingSaysHowToConfigure(t *testing.T) {
	_, err := LoadEnvFile(filepath.Join(t.TempDir(), "absent.env"))
	if err == nil || !strings.Contains(err.Error(), "hotserve backup init") {
		t.Fatalf("want an error naming init, got %v", err)
	}
}

func TestLoadEnvFileRejectsRubbish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.env")
	if err := os.WriteFile(path, []byte("RESTIC_REPOSITORY s3:example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEnvFile(path); err == nil || !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Fatalf("want a KEY=VALUE error, got %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.env")
	if err := os.WriteFile(empty, []byte("# nothing here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEnvFile(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("want an empty-file error, got %v", err)
	}
}

// The run creates staging dirs as root, but the job writes its
// database copies as the hotserve user: without the chown every
// VACUUM INTO fails with a permission error.
func TestEnsureStagingDirIsOwnedByTheJobsUser(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	anchor := t.TempDir()
	dir := filepath.Join(anchor, "blog")
	if err := ensureStagingDir(anchor, "blog", me.Username); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for _, d := range []string{StagingData(dir), StagingCache(dir)} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Errorf("%s must be made up front — the job cannot, from inside its view: %v", d, err)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("mode = %o, want 750", got)
	}
	// Chown to a user that does not exist must name the user, not
	// fail with a bare errno.
	err = ensureStagingDir(t.TempDir(), "x", "no-such-user-here")
	if err == nil || !strings.Contains(err.Error(), "no-such-user-here") {
		t.Fatalf("want an error naming the user, got %v", err)
	}
}

// What is under the staging root is written by jobs, and this runs as
// root: a link a job left there — where a directory should be, or on
// the way to one — must never take the mkdir and the chown outside the
// staging root.
func TestEnsureStagingDirNeverLeavesItsAnchor(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(anchor, outside string) error{
		"data is a link out": func(anchor, outside string) error {
			return os.Symlink(outside, filepath.Join(anchor, "blog", "data"))
		},
		"the restore dir is a link out": func(anchor, outside string) error {
			return os.Symlink(outside, filepath.Join(anchor, "blog", "restore"))
		},
		"the restore dir is a link to a sibling app": func(anchor, _ string) error {
			if err := os.Mkdir(filepath.Join(anchor, "shop"), 0o750); err != nil {
				return err
			}
			return os.Symlink("../shop", filepath.Join(anchor, "blog", "restore"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			anchor, outside := t.TempDir(), t.TempDir()
			if err := os.Mkdir(filepath.Join(anchor, "blog"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := plant(anchor, outside); err != nil {
				t.Fatal(err)
			}
			err := ensureStagingDir(anchor, "blog", me.Username)
			if err == nil {
				err = ensureStagingDir(anchor, filepath.Join("blog", stagingRestore), me.Username)
			}
			if err == nil {
				t.Fatal("a link where a staging dir should be must be refused")
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Errorf("something was made outside the staging root: %v", entries)
			}
			if entries, _ := os.ReadDir(filepath.Join(anchor, "shop")); len(entries) != 0 {
				t.Errorf("something was made in another app's dir: %v", entries)
			}
		})
	}
}
