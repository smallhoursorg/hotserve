package backup

import (
	"os"
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
