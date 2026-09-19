package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --credentials-file is this command's own format: KEY=VALUE lines,
// # comments and blank lines, a value's surrounding quotes dropped. (The
// settings file is systemd's, and nothing here reads it.)
func TestCredentialsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s3-key")
	body := "# from the provider's console\n\nAWS_ACCESS_KEY_ID=plain\nAWS_SECRET_ACCESS_KEY=\"secret=with=equals\"\nB2_ACCOUNT_KEY='quoted'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := CredentialsFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"AWS_ACCESS_KEY_ID=plain", "AWS_SECRET_ACCESS_KEY=secret=with=equals", "B2_ACCOUNT_KEY=quoted"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q, want %q", env, want)
	}
}

// What goes wrong with it is said in its own terms: telling someone to
// run `init` to create the file they are passing to init is a loop.
func TestCredentialsFileSaysWhatIsWrongWithIt(t *testing.T) {
	_, err := CredentialsFile(filepath.Join(t.TempDir(), "absent"))
	if err == nil || !strings.Contains(err.Error(), "a file you write") {
		t.Errorf("a missing file: %v", err)
	}
	rubbish := filepath.Join(t.TempDir(), "rubbish")
	if err := os.WriteFile(rubbish, []byte("AWS_ACCESS_KEY_ID plain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CredentialsFile(rubbish); err == nil || !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Errorf("a line that is not KEY=VALUE: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("# nothing here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CredentialsFile(empty); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("an empty file: %v", err)
	}
}
