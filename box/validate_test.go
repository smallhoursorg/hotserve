package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/hotserve/liveswap"
)

// stubValidator is the real child runner over stub programs: script is
// the body of /bin/sh scripts standing for `hotserve` and, unless "",
// `hotserve-backup`. Each script records its arguments and the staged
// file beside itself.
func stubValidator(t *testing.T, hotserve, backup string, env ...string) (*childValidator, string) {
	t.Helper()
	dir := t.TempDir()
	stage := filepath.Join(dir, "stage")
	must(t, os.Mkdir(stage, 0o700))
	c := &childValidator{
		stage: stage, hotserve: script(t, dir, "hotserve", hotserve), backup: filepath.Join(dir, "absent"),
		env: env, timeout: 5 * time.Second, redactor: liveswap.NewRedactor(secretCandidates(env), nil),
	}
	if backup != "" {
		c.backup = script(t, dir, "hotserve-backup", backup)
	}
	return c, dir
}

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	rec := filepath.Join(dir, name+".args")
	// The last argument is the staged file: copy what it held while
	// the child ran.
	text := "#!/bin/sh\necho \"$@\" > " + rec + "\nfor a; do last=$a; done\ncat \"$last\" > " + rec + ".file 2>/dev/null\n" + body + "\n"
	must(t, os.WriteFile(path, []byte(text), 0o755))
	return path
}

func TestValidateChild(t *testing.T) {
	file := []byte("example.com {\n\trespond hi\n}\n")
	t.Run("passes", func(t *testing.T) {
		c, dir := stubValidator(t, "exit 0", "")
		msg, err := c.validate(context.Background(), "0123456789abcdef0123456789abcdef", file)
		if msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
		args, _ := os.ReadFile(filepath.Join(dir, "hotserve.args"))
		staged := filepath.Join(c.stage, stagedConfig("0123456789abcdef0123456789abcdef"))
		if strings.TrimSpace(string(args)) != "validate --adapter caddyfile --config "+staged {
			t.Fatalf("args %q", args)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "hotserve.args.file")); string(got) != string(file) {
			t.Fatalf("the child read %q", got)
		}
		if exists(staged) {
			t.Fatal("the staged copy was left behind")
		}
	})
	t.Run("refuses with the error line", func(t *testing.T) {
		c, _ := stubValidator(t, `echo '{"level":"info","msg":"using config"}' >&2; echo 'Error: adapting config using caddyfile: Caddyfile:2: unrecognized directive: respnd' >&2; exit 1`, "")
		msg, err := c.validate(context.Background(), randomID(t), file)
		if err != nil || msg != "hotserve validate: adapting config using caddyfile: Caddyfile:2: unrecognized directive: respnd" {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("redacts the environment's values", func(t *testing.T) {
		secret := "pg-pass-" + strings.Repeat("q7", 10)
		// Quoted bare, as an expanded placeholder is: only the layer
		// primed with the environment can know it.
		c, _ := stubValidator(t, `echo "Error: parsing port: $DB_PASS is not a number"; exit 1`, "", "DB_PASS="+secret, "HOME=/var/lib/hotserve")
		msg, err := c.validate(context.Background(), randomID(t), file)
		if err != nil || strings.Contains(msg, secret) || !strings.HasPrefix(msg, "hotserve validate: parsing port: ") {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("bounds the line", func(t *testing.T) {
		c, _ := stubValidator(t, `printf 'Error: '; head -c 2000 /dev/zero | tr '\0' 'x'; echo; exit 1`, "")
		msg, _ := c.validate(context.Background(), randomID(t), file)
		if len(msg) > len("hotserve validate: ")+300+len("...") { // proof.Bound's cap
			t.Fatalf("%d bytes", len(msg))
		}
	})
	t.Run("keeps the tail of a long output", func(t *testing.T) {
		c, _ := stubValidator(t, `head -c 100000 /dev/zero | tr '\0' 'y'; echo; echo 'Error: the end'; exit 1`, "")
		msg, _ := c.validate(context.Background(), randomID(t), file)
		if msg != "hotserve validate: the end" {
			t.Fatalf("msg %q", msg)
		}
	})
	t.Run("no output", func(t *testing.T) {
		c, _ := stubValidator(t, "exit 3", "")
		msg, err := c.validate(context.Background(), randomID(t), file)
		if err != nil || msg != "hotserve validate: exit status 3" {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("a child that does not finish", func(t *testing.T) {
		c, _ := stubValidator(t, "exec sleep 60", "")
		c.timeout = 200 * time.Millisecond
		start := time.Now()
		msg, err := c.validate(context.Background(), randomID(t), file)
		if err != nil || msg != "hotserve validate: did not finish within 200ms" || time.Since(start) > 10*time.Second {
			t.Fatalf("msg %q err %v after %s", msg, err, time.Since(start))
		}
	})
	t.Run("a grandchild holding the output", func(t *testing.T) {
		// The child exits; a grandchild keeps the pipe. WaitDelay ends
		// the wait instead of the grandchild's sleep.
		c, _ := stubValidator(t, "sleep 60 & echo 'Error: gone'; exit 1", "")
		start := time.Now()
		msg, err := c.validate(context.Background(), randomID(t), file)
		if err != nil || msg != "hotserve validate: gone" || time.Since(start) > 10*time.Second {
			t.Fatalf("msg %q err %v after %s", msg, err, time.Since(start))
		}
	})
	t.Run("a program that cannot start is the box's", func(t *testing.T) {
		c, _ := stubValidator(t, "exit 0", "")
		c.hotserve = filepath.Join(t.TempDir(), "missing")
		if msg, err := c.validate(context.Background(), randomID(t), file); msg != "" || err == nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("the caller gone is not a verdict", func(t *testing.T) {
		c, _ := stubValidator(t, "exec sleep 60", "")
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		if msg, err := c.validate(ctx, randomID(t), file); msg != "" || err == nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
}

func TestValidateBackup(t *testing.T) {
	file := []byte("example.com\n")
	t.Run("installed and refusing", func(t *testing.T) {
		c, dir := stubValidator(t, "exit 0", "echo 'Error: backup app is not declared' >&2; exit 2")
		id := randomID(t)
		msg, err := c.validate(context.Background(), id, file)
		if err != nil || msg != "hotserve-backup validate: backup app is not declared" {
			t.Fatalf("msg %q err %v", msg, err)
		}
		args, _ := os.ReadFile(filepath.Join(dir, "hotserve-backup.args"))
		if strings.TrimSpace(string(args)) != "validate "+filepath.Join(c.stage, stagedConfig(id)) {
			t.Fatalf("args %q", args)
		}
	})
	t.Run("installed and passing", func(t *testing.T) {
		c, _ := stubValidator(t, "exit 0", "exit 0")
		if msg, err := c.validate(context.Background(), randomID(t), file); msg != "" || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
	})
	t.Run("not after hotserve refused", func(t *testing.T) {
		c, dir := stubValidator(t, "echo 'Error: no'; exit 1", "exit 0")
		if msg, _ := c.validate(context.Background(), randomID(t), file); msg != "hotserve validate: no" {
			t.Fatalf("msg %q", msg)
		}
		if exists(filepath.Join(dir, "hotserve-backup.args")) {
			t.Fatal("backup validate ran after hotserve's refusal")
		}
	})
	t.Run("could not tell", func(t *testing.T) {
		c, dir := stubValidator(t, "exit 0", "")
		c.backup = filepath.Join(dir, "not-executable")
		must(t, os.WriteFile(c.backup, []byte("#!/bin/sh\n"), 0o644))
		if msg, err := c.validate(context.Background(), randomID(t), file); msg != msgBackupUnknown || err != nil {
			t.Fatalf("msg %q err %v", msg, err)
		}
		c.backup = filepath.Join(dir, "hotserve.args", "x") // a component that is not a directory
		if msg, _ := c.validate(context.Background(), randomID(t), file); msg != msgBackupUnknown {
			t.Fatalf("ENOTDIR: msg %q", msg)
		}
	})
}

func TestErrorLine(t *testing.T) {
	for out, want := range map[string]string{
		"":                            "",
		"\n\n":                        "",
		"one\ntwo\n":                  "two",
		"Error: first\nlater noise\n": "first",
		"Error: a\nError: b\n\n":      "b",
		"  indented last  \n":         "indented last",
		"x Error: not at the start\n": "x Error: not at the start",
	} {
		if got := errorLine([]byte(out)); got != want {
			t.Errorf("errorLine(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestTailBuffer(t *testing.T) {
	var b tailBuffer
	big := strings.Repeat("a", maxValidateOutput) + "END"
	for _, chunk := range []string{"start ", big[:100], big[100:], "\nlast"} {
		if n, err := b.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatal(n, err)
		}
	}
	if len(b.b) != maxValidateOutput || !strings.HasSuffix(string(b.b), "END\nlast") {
		t.Fatalf("%d bytes, ends %q", len(b.b), string(b.b[len(b.b)-10:]))
	}
	b = tailBuffer{}
	_, _ = b.Write([]byte(big))
	if len(b.b) != maxValidateOutput || !strings.HasSuffix(string(b.b), "END") {
		t.Fatal("one large write")
	}
}

func TestSecretCandidates(t *testing.T) {
	got := secretCandidates([]string{"PATH=/usr/bin:/bin", "HOME=/var/lib/hotserve", "TOKEN=abc", "EMPTY=", "NOEQUALS"})
	if strings.Join(got, ",") != "TOKEN=abc,EMPTY=" {
		t.Fatalf("%v", got)
	}
}
