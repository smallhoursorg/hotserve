package liveswap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// After a restart the app is reattached without a launch: the first
// filter built for it reads the env_file from the spec, so a response
// never carries a value the file holds, and a later launch adds to
// what is known rather than replacing it.
func TestRedactorForLoadsEnvFileWithoutALaunch(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(envFile, []byte("DATABASE_URL=postgres://app:s3cr3t-pw-value@db/app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec(t)
	spec.envFile = envFile
	ma := newManagedApp(spec.name)
	ma.spec = spec

	out, keys := ma.redactorFor(statusSnapshot{}).redact("dial postgres://app:s3cr3t-pw-value@db/app: refused")
	if strings.Contains(out, "s3cr3t-pw-value") || strings.Join(keys, ",") != "DATABASE_URL" {
		t.Fatalf("env_file value not known before any launch: %q %v", out, keys)
	}

	// A launch with a rotated value: both are known afterwards.
	ma.rememberSecrets(envFile, []string{"DATABASE_URL=postgres://app:new-pw-value-2@db/app"})
	out, _ = ma.redactorFor(statusSnapshot{}).redact("old s3cr3t-pw-value new new-pw-value-2")
	if strings.Contains(out, "s3cr3t-pw-value") || strings.Contains(out, "new-pw-value-2") {
		t.Fatalf("a rotated value must stay redacted: %q", out)
	}

	// A reload that names a different env_file: the next filter reads
	// it, and what the old one held stays known.
	other := filepath.Join(t.TempDir(), "other.env")
	if err := os.WriteFile(other, []byte("TOKEN=from-the-other-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := testSpec(t)
	reloaded.envFile = other
	ma.spec = reloaded
	out, _ = ma.redactorFor(statusSnapshot{}).redact("from-the-other-file and s3cr3t-pw-value")
	if strings.Contains(out, "from-the-other-file") || strings.Contains(out, "s3cr3t-pw-value") {
		t.Fatalf("after a reload naming another env_file: %q", out)
	}

	// A file that cannot be read yet is not an error here, and the
	// read is retried on the next response until it succeeds.
	spec2 := testSpec(t)
	spec2.envFile = filepath.Join(t.TempDir(), "late.env")
	ma2 := newManagedApp(spec2.name)
	ma2.spec = spec2
	if out, keys := ma2.redactorFor(statusSnapshot{}).redact("plain"); out != "plain" || len(keys) != 0 {
		t.Fatalf("missing env_file: %q %v", out, keys)
	}
	if err := os.WriteFile(spec2.envFile, []byte("TOKEN=late-but-known-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := ma2.redactorFor(statusSnapshot{}).redact("saw late-but-known-value"); strings.Contains(out, "late-but-known-value") {
		t.Fatalf("env_file written after the first response was never read: %q", out)
	}

	// A value equal to one of the app's own paths, or its name, is not
	// a secret whatever file it came from.
	spec3 := testSpec(t)
	spec3.envFile = filepath.Join(t.TempDir(), "paths.env")
	if err := os.WriteFile(spec3.envFile, []byte("APP_ROOT="+spec3.dirs.app+"\nNAME="+spec3.name+"-is-short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ma3 := newManagedApp(spec3.name)
	ma3.spec = spec3
	if out, keys := ma3.redactorFor(statusSnapshot{}).redact("socket " + spec3.dirs.app + "/run/x/app.sock"); strings.Contains(out, "[redacted") || len(keys) != 0 {
		t.Fatalf("the app's own path was redacted: %q %v", out, keys)
	}
}
