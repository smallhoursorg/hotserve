package examples

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// The box template's secrets commands — bin/secrets behind make secret,
// secrets, secret-show, rekey and secrets-import, and bin/signer's
// reader line — in a repository made from examples/box, with the age
// CLI built here and every key made at test time.

var (
	ageOnce sync.Once
	ageDir  string
	ageErr  error
)

// ageVersion is the age the tests encrypt and decrypt with: Homebrew's
// at the time of writing. Debian 13 ships 1.2.1, which reads and writes
// the same files.
const ageVersion = "v1.3.2"

// ageTools builds age and age-keygen once, outside this module's
// graph: the template's commands run the age CLI, not hotserve's code.
func ageTools(t *testing.T) string {
	t.Helper()
	ageOnce.Do(func() {
		dir, err := os.MkdirTemp("", "age-bin")
		if err != nil {
			ageErr = err
			return
		}
		ageDir = dir // TestMain removes it, built or not
		cmd := exec.Command("go", "install",
			"filippo.io/age/cmd/age@"+ageVersion, "filippo.io/age/cmd/age-keygen@"+ageVersion)
		cmd.Env = append(os.Environ(), "GOBIN="+dir, "GOWORK=off", "GOFLAGS=")
		if out, err := cmd.CombinedOutput(); err != nil {
			ageErr = fmt.Errorf("go install age %s: %w\n%s", ageVersion, err, out)
		}
	})
	if ageErr != nil {
		t.Fatal(ageErr)
	}
	return ageDir
}

// secretsRepo is a box repository with the age CLI on its PATH.
func secretsRepo(t *testing.T) *boxRepo {
	t.Helper()
	dir := ageTools(t)
	r := newBoxRepo(t)
	r.env = append(r.env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return r
}

// secrets runs bin/secrets <cmd> with stdin and env, under the shell
// given (the script's own #!/bin/sh when shell is "").
func (r *boxRepo) secrets(shell, stdin, cmd string, env ...string) (string, int) {
	r.t.Helper()
	script := filepath.Join(r.dir, "bin", "secrets")
	c := exec.Command(script, cmd)
	if shell != "" {
		c = exec.Command(shell, script, cmd)
	}
	c.Dir, c.Env = r.dir, append(append([]string{}, r.env...), env...)
	c.Stdin = strings.NewReader(stdin)
	out, err := c.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		r.t.Fatalf("bin/secrets %s: %v\n%s", cmd, err, out)
	}
	return string(out), 0
}

// boxKey makes an age identity, as hotserve box key would on a box, and
// returns its file and its recipient.
func (r *boxRepo) boxKey(name string) (string, string) {
	r.t.Helper()
	id := filepath.Join(r.home, name+".age-identity")
	keygen := filepath.Join(ageTools(r.t), "age-keygen")
	if out, err := exec.Command(keygen, "-o", id).CombinedOutput(); err != nil {
		r.t.Fatalf("age-keygen: %v\n%s", err, out)
	}
	out, err := exec.Command(keygen, "-y", id).Output()
	if err != nil {
		r.t.Fatalf("age-keygen -y: %v", err)
	}
	return id, strings.TrimSpace(string(out))
}

// decrypt reads an encrypted value with an identity file, or reports
// that it could not.
func (r *boxRepo) decrypt(identity, rel string) (string, error) {
	r.t.Helper()
	c := exec.Command(filepath.Join(ageTools(r.t), "age"), "-d", "-i", identity, filepath.Join(r.dir, rel))
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, stderr.String())
	}
	return string(out), nil
}

func (r *boxRepo) mustDecrypt(identity, rel, want string) {
	r.t.Helper()
	got, err := r.decrypt(identity, rel)
	if err != nil {
		r.t.Fatalf("%s with %s: %v", rel, filepath.Base(identity), err)
	}
	if got != want {
		r.t.Fatalf("%s with %s = %q, want %q", rel, filepath.Base(identity), got, want)
	}
}

func (r *boxRepo) rsaKey(name string, bits int) string {
	r.t.Helper()
	priv := filepath.Join(r.home, ".ssh", name)
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "rsa", "-b", fmt.Sprint(bits), "-N", "", "-C", name, "-f", priv).CombinedOutput(); err != nil {
		r.t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	return priv + ".pub"
}

func (r *boxRepo) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(r.dir, rel))
	return err == nil
}

// shortKey is how bin/secrets names a box's key in a message.
func shortKey(k string) string { return k[:8] + "…" + k[len(k)-4:] }

const (
	recipientsFile = "deploy.example.com/secrets/recipients"
	dbFile         = "deploy.example.com/secrets/example/DATABASE_URL.age"
)

// signers: alice (ed25519) and carol (RSA 2048) are readers; bob (a
// hardware sk- key) and dave (RSA 1024) cannot be, and are told so.
func readersRepo(t *testing.T) (r *boxRepo, alice, carol string) {
	t.Helper()
	r = secretsRepo(t)
	carolPub := r.rsaKey("carol", 2048)
	davePub := r.rsaKey("dave", 1024)
	for _, s := range [][]string{{carolPub, "carol"}, {davePub, "dave"}} {
		if out, code := r.run("signer", "KEY="+s[0], "PRINCIPAL="+s[1]); code != 0 {
			t.Fatalf("make signer %s: exit %d\n%s", s[1], code, out)
		}
	}
	// No hardware here to make an sk- key with: its line by hand. A key
	// of a type age cannot take is never read, only named.
	f := r.read(boxFile)
	r.write(boxFile, strings.Replace(f, "\t\t# signers\n",
		"\t\t# signers\n\t\tsigner bob@example.com sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIA==\n", 1))
	alicePub := r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 { // last, so the checkout signs, and reads, as alice
		t.Fatalf("make signer: exit %d\n%s", code, out)
	}
	return r, strings.TrimSuffix(alicePub, ".pub"), strings.TrimSuffix(carolPub, ".pub")
}

func TestBoxSecretSet(t *testing.T) {
	r, alice, carol := readersRepo(t)
	app := []string{"APP=example", "VAR=DATABASE_URL"}

	// The box's first secret needs the box's own key.
	if out, code := r.secrets("", "v\n", "set", app...); code != 1 ||
		!strings.Contains(out, "this first one needs the box's own key: BOX_KEY=$(ssh root@<your box> hotserve box key)") {
		t.Fatalf("no BOX_KEY: exit %d\n%s", code, out)
	}
	for _, bad := range []string{"age1nope", "ssh-ed25519 AAAA", "age1pq1" + strings.Repeat("q", 55), "AGE-SECRET-KEY-1" + strings.Repeat("Q", 58)} {
		if out, code := r.secrets("", "v\n", "set", append(app, "BOX_KEY="+bad)...); code != 1 || !strings.Contains(out, "BOX_KEY is not a box's key") {
			t.Fatalf("BOX_KEY=%s: exit %d\n%s", bad, code, out)
		}
	}
	if r.exists(recipientsFile) {
		t.Fatal("a refused first secret made the recipients file")
	}

	boxID, box := r.boxKey("box")
	// A first value refused leaves no recipients file behind either.
	if out, code := r.secrets("", "", "set", append(app, "BOX_KEY="+box)...); code != 1 || !strings.Contains(out, "DATABASE_URL is empty") || r.exists(recipientsFile) {
		t.Fatalf("a refused first value: exit %d\n%s", code, out)
	}
	out, code := r.secrets("", "postgres://u:p@db/app\n", "set", append(app, "BOX_KEY="+box+"\r")...) // ssh -t adds a CR
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"secret: made " + recipientsFile + ": this box (" + shortKey(box) + "), alice@example.com, carol\n",
		"secret: bob@example.com signs with a sk-ssh-ed25519@openssh.com key, which age cannot encrypt to; to read secrets they add an age key of their own",
		"secret: dave signs with an RSA key under 2048 bits, which age does not take;",
		"secret: " + dbFile + ", encrypted to 3 keys, added to the commit\n",
		"secret: example's block does not read its secrets yet; add this line to it:\nsecret:     env_file /etc/hotserve/secrets/example.env\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, id := range []string{boxID, alice, carol} {
		r.mustDecrypt(id, dbFile, "postgres://u:p@db/app")
	}
	rec := r.read(recipientsFile)
	if first := firstKey(rec); first != box {
		t.Errorf("the recipients file's first key is %q, want the box's %q:\n%s", first, box, rec)
	}
	if !strings.Contains(rec, "# alice@example.com\nssh-ed25519 "+r.b64(alice+".pub")+"\n") || strings.Contains(rec, "dave") || strings.Contains(rec, "bob@") {
		t.Errorf("the recipients file:\n%s", rec)
	}
	if got := r.git("diff", "--cached", "--name-only"); !strings.Contains(got, dbFile) || !strings.Contains(got, recipientsFile) {
		t.Errorf("staged: %q", got)
	}

	// Again: the value replaced, the recipients file kept.
	if out, code := r.secrets("", "postgres://u:p@db/app2", "set", app...); code != 0 || !strings.Contains(out, dbFile+", encrypted to 3 keys: its new value added to the commit") {
		t.Fatalf("a second value: exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, dbFile, "postgres://u:p@db/app2")
	if r.read(recipientsFile) != rec {
		t.Error("a second value changed the recipients file")
	}
	// A BOX_KEY that is not the recipients file's box is a rekey, not a set.
	_, other := r.boxKey("other")
	if out, code := r.secrets("", "x", "set", append(app, "BOX_KEY="+other)...); code != 1 || !strings.Contains(out, "a rebuilt box's secrets move to its new key with make rekey BOX_KEY="+other) {
		t.Fatalf("another BOX_KEY: exit %d\n%s", code, out)
	}

	// What the value may be: what an env file can carry, one line of
	// UTF-8. One trailing newline is dropped, nothing else is changed.
	for _, c := range []struct{ in, want string }{
		{"x\r\n", "x"},
		{"  \"quoted\" and 'single'  ", "  \"quoted\" and 'single'  "},
		{"#=a=b # c", "#=a=b # c"},
		{"caf" + string(rune(0xe9)) + " " + string(rune(0x1f600)) + string(rune(0x3000)), "caf" + string(rune(0xe9)) + " " + string(rune(0x1f600)) + string(rune(0x3000))},
		// The longest whose own line, EDGE="…" and its newline, fits
		// the box's 64 KiB env file.
		{strings.Repeat("v", 65536-len("EDGE")-4), strings.Repeat("v", 65536-len("EDGE")-4)},
	} {
		if out, code := r.secrets("", c.in, "set", "APP=example", "VAR=EDGE"); code != 0 {
			t.Fatalf("value %q: exit %d\n%s", c.in, code, out)
		}
		r.mustDecrypt(boxID, "deploy.example.com/secrets/example/EDGE.age", c.want)
	}
	before := r.read("deploy.example.com/secrets/example/EDGE.age")
	for _, c := range []struct{ in, want string }{
		{"", "EDGE is empty"},
		{"\n", "EDGE is empty"},
		{"a\nb", "EDGE has a line break in it"},
		{"a\n\n", "EDGE has a line break in it"},
		{"a\rb", "EDGE has a carriage return in it"},
		{"a\x00b", "EDGE has a NUL byte in it"},
		{"\xff", "EDGE is not UTF-8 text"},
		{"\xed\xa0\x80", "EDGE is not UTF-8 text"}, // a surrogate
		{"\xc0\xaf", "EDGE is not UTF-8 text"},     // overlong
		{"\xe2\x82", "EDGE is not UTF-8 text"},     // cut short
		{strings.Repeat("v", 65536-len("EDGE")-3), "EDGE is too long: its line in the env file would be over 65536 bytes"},
	} {
		if out, code := r.secrets("", c.in, "set", "APP=example", "VAR=EDGE"); code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("value %q: exit %d, want %q\n%s", c.in, code, c.want, out)
		}
	}
	if r.read("deploy.example.com/secrets/example/EDGE.age") != before {
		t.Error("a refused value replaced the one there")
	}

	// Names.
	for _, c := range []struct {
		env  []string
		want string
	}{
		{[]string{"APP=example", "VAR=1X"}, "variable '1X': letters, digits and _ only, not starting with a digit"},
		{[]string{"APP=example", "VAR=A-B"}, "variable 'A-B': letters, digits and _ only"},
		{[]string{"APP=example", "VAR=" + strings.Repeat("V", 129)}, "at most 128 bytes"},
		{[]string{"APP=example"}, "name the variable: VAR=DATABASE_URL"},
		{[]string{"VAR=X"}, "name the app: APP=example"},
		{[]string{"APP=Example", "VAR=X"}, "app 'Example': lower-case letters, digits and - only"},
		{[]string{"APP=recipients", "VAR=X"}, "that name is the recipients file's"},
		{[]string{"APP=" + strings.Repeat("a", 64), "VAR=X"}, "at most 63 characters"},
		{[]string{"APP=example", "VAR=X", "BOX=nope.example.com"}, "no box nope.example.com here"},
	} {
		if out, code := r.secrets("", "v", "set", c.env...); code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("%v: exit %d, want %q\n%s", c.env, code, c.want, out)
		}
	}

	// The block that reads them: no hint once the line is there.
	f := r.read(boxFile)
	r.write(boxFile, strings.Replace(f, "# env_file /etc/hotserve/secrets/example.env", "env_file /etc/hotserve/secrets/example.env", 1))
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=TOKEN"); code != 0 || strings.Contains(out, "env_file") {
		t.Errorf("with the env_file line: exit %d\n%s", code, out)
	}
	r.write(boxFile, strings.Replace(f, "# env_file /etc/hotserve/secrets/example.env", "env_file /etc/hotserve/example.env", 1))
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=TOKEN"); code != 0 || !strings.Contains(out, "example's block reads env_file /etc/hotserve/example.env; to read its secrets it needs:\nsecret:     env_file /etc/hotserve/secrets/example.env") {
		t.Errorf("another env_file: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "v", "set", "APP=blog", "VAR=TOKEN"); code != 0 || !strings.Contains(out, boxFile+" has no app blog yet: the box takes secrets only for an app its file declares") {
		t.Errorf("an app the file does not declare: exit %d\n%s", code, out)
	}
	r.write(boxFile, f)

	// On a Mac's disk two names that differ in case are one file: the
	// second would overwrite the first.
	r.write("deploy.example.com/secrets/example/Database_URL.age", "x")
	if out, code := r.secrets("", "v", "set", app...); code != 1 || !strings.Contains(out, "deploy.example.com/secrets/example/Database_URL.age differs from DATABASE_URL.age only in case: on a Mac's default disk they are one file") {
		t.Errorf("a case twin: exit %d\n%s", code, out)
	}
	if r.read("deploy.example.com/secrets/example/Database_URL.age") != "x" {
		t.Error("the case twin was overwritten")
	}
	r.remove("deploy.example.com/secrets/example/Database_URL.age")

	// An app has at most 256 variables; one already there can change.
	for i := 0; i < 256; i++ {
		r.write(fmt.Sprintf("deploy.example.com/secrets/many/V%03d.age", i), "x")
	}
	if out, code := r.secrets("", "v", "set", "APP=many", "VAR=ONE_MORE"); code != 1 || !strings.Contains(out, "many has 256 variables, the box's cap for an app") {
		t.Errorf("the 257th variable: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "v", "set", "APP=many", "VAR=V000"); code != 0 {
		t.Errorf("a change at the cap: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "V999=1\n", "import", "APP=many"); code != 1 || !strings.Contains(out, "many would have more than 256 variables") {
		t.Errorf("an import past the cap: exit %d\n%s", code, out)
	}
	r.remove("deploy.example.com/secrets/many")

	// The recipients file's first key is the box's, or nothing is
	// encrypted to it.
	rec = r.read(recipientsFile)
	r.write(recipientsFile, "# alice first\nssh-ed25519 "+r.b64(alice+".pub")+"\n"+rec)
	if out, code := r.secrets("", "v", "set", app...); code != 1 || !strings.Contains(out, "the first key in the recipients file is not a box's") {
		t.Errorf("a reader first: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "", "rekey", "IDENTITY="+alice); code != 1 || !strings.Contains(out, "the first key in the recipients file is not a box's") {
		t.Errorf("rekey with a reader first: exit %d\n%s", code, out)
	}
	r.write(recipientsFile, rec)

	// More than one box: BOX names it.
	r.write("deploy2.example.com/Caddyfile", strings.Replace(f, "deploy.example.com {", "deploy2.example.com {", 1))
	if out, code := r.secrets("", "v", "set", app...); code != 1 || !strings.Contains(out, "more than one box; name one with BOX=: deploy.example.com deploy2.example.com") {
		t.Errorf("two boxes: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "v", "set", append(app, "BOX=deploy.example.com/")...); code != 0 {
		t.Errorf("BOX=: exit %d\n%s", code, out)
	}
	// A box tracked but deleted here is on its way out, as make signer
	// and make check read it: the other is then the only one.
	r.commit("a second box")
	r.remove("deploy2.example.com")
	if out, code := r.secrets("", "v", "set", app...); code != 0 {
		t.Errorf("a deleted second box: exit %d\n%s", code, out)
	}
}

// The shells it runs under: the container's /bin/sh is dash; a Mac's is
// bash 3.2 in POSIX mode.
func TestBoxSecretSetUnderBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash here")
	}
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("make signer: exit %d\n%s", code, out)
	}
	boxID, box := r.boxKey("box")
	if out, code := r.secrets("bash", "v1\n", "set", "APP=example", "VAR=A", "BOX_KEY="+box); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, "deploy.example.com/secrets/example/A.age", "v1")
	if out, code := r.secrets("bash", "B=two\n", "import", "APP=example"); code != 0 {
		t.Fatalf("import: exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, "deploy.example.com/secrets/example/B.age", "two")
}

func firstKey(recipients string) string {
	for _, l := range strings.Split(recipients, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return strings.Fields(l)[0]
		}
	}
	return ""
}

func TestBoxSecretsListShowRekey(t *testing.T) {
	r, alice, carol := readersRepo(t)
	boxID, box := r.boxKey("box")
	if out, code := r.secrets("", "one", "set", "APP=example", "VAR=DATABASE_URL", "BOX_KEY="+box); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "two", "set", "APP=example", "VAR=STRIPE_KEY"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}

	// The names and the readers, with no key.
	out, code := r.secrets("", "", "list")
	if want := "deploy.example.com\n  readers: this box (" + shortKey(box) + "), alice@example.com, carol\n  example: DATABASE_URL STRIPE_KEY\n"; code != 0 || out != want {
		t.Fatalf("list: exit %d\n%s\nwant:\n%s", code, out, want)
	}
	// A signer whose key age takes, not in the file: named.
	erin := r.key("erin")
	f := r.read(boxFile)
	r.write(boxFile, strings.Replace(f, "\t\t# signers\n", "\t\t# signers\n\t\tsigner erin ssh-ed25519 "+r.b64(erin)+"\n", 1))
	if out, _ := r.secrets("", "", "list"); !strings.Contains(out, "  erin signs but is not a reader: their make signer adds them, then a reader runs make rekey\n") {
		t.Errorf("a signer who is not a reader:\n%s", out)
	}
	// The last signer a hardware key, which can never be a reader: still
	// a clean exit.
	carolLine := "signer carol ssh-rsa " + r.b64(carol+".pub") + "\n"
	if !strings.Contains(f, carolLine) {
		t.Fatalf("no carol line in:\n%s", f)
	}
	r.write(boxFile, strings.Replace(f, carolLine, carolLine+"\t\tsigner zed sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIA==\n", 1))
	if out, code := r.secrets("", "", "list"); code != 0 {
		t.Errorf("list with an sk- signer last: exit %d\n%s", code, out)
	}
	r.write(boxFile, f)

	// Read back: with IDENTITY, else the key this checkout signs with.
	if out, code := r.secrets("", "", "show", "APP=example", "VAR=STRIPE_KEY"); code != 0 || out != "two" {
		t.Errorf("show as the signing key: exit %d %q", code, out)
	}
	if out, code := r.secrets("", "", "show", "APP=example", "VAR=STRIPE_KEY", "IDENTITY="+carol); code != 0 || out != "two" {
		t.Errorf("show with IDENTITY: exit %d %q", code, out)
	}
	if out, code := r.secrets("", "", "show", "APP=example", "VAR=NOPE"); code != 1 || !strings.Contains(out, "no deploy.example.com/secrets/example/NOPE.age: make secrets lists what there is") {
		t.Errorf("show of nothing: exit %d\n%s", code, out)
	}
	r.git("config", "--unset", "user.signingkey")
	if out, code := r.secrets("", "", "show", "APP=example", "VAR=STRIPE_KEY"); code != 1 || !strings.Contains(out, "no private key to read with: name yours with IDENTITY=") {
		t.Errorf("show with no key: exit %d\n%s", code, out)
	}

	// A rebuilt box: every value to its new key, as a reader.
	newID, newBox := r.boxKey("rebuilt")
	out, code = r.secrets("", "", "rekey", "BOX_KEY="+newBox, "IDENTITY="+alice)
	if code != 0 || !strings.Contains(out, "rekey: "+recipientsFile+" now names the box as "+shortKey(newBox)+"\n") ||
		!strings.Contains(out, "rekey: deploy.example.com: 2 secrets encrypted again to 3 keys, in the commit\n") {
		t.Fatalf("rekey: exit %d\n%s", code, out)
	}
	if first := firstKey(r.read(recipientsFile)); first != newBox {
		t.Errorf("first key after rekey: %q", first)
	}
	for _, f := range []string{dbFile, "deploy.example.com/secrets/example/STRIPE_KEY.age"} {
		r.mustDecrypt(newID, f, map[bool]string{true: "one", false: "two"}[f == dbFile])
		if _, err := r.decrypt(boxID, f); err == nil {
			t.Errorf("%s still opens with the old box's key", f)
		}
		r.mustDecrypt(carol, f, map[bool]string{true: "one", false: "two"}[f == dbFile])
	}

	// A value this reader cannot open: nothing changes.
	c := exec.Command(filepath.Join(ageTools(t), "age"), "-r", newBox, "-o", filepath.Join(r.dir, "deploy.example.com/secrets/example/BOX_ONLY.age"))
	c.Stdin = strings.NewReader("box only")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("age: %v\n%s", err, out)
	}
	snap := map[string]string{}
	for _, f := range []string{dbFile, "deploy.example.com/secrets/example/STRIPE_KEY.age", recipientsFile} {
		snap[f] = r.read(f)
	}
	_, third := r.boxKey("third")
	if out, code := r.secrets("", "", "rekey", "BOX_KEY="+third, "IDENTITY="+alice); code != 1 ||
		!strings.Contains(out, "could not read deploy.example.com/secrets/example/BOX_ONLY.age with "+alice+", so nothing changed") {
		t.Fatalf("rekey of a value it cannot read: exit %d\n%s", code, out)
	}
	for f, b := range snap {
		if r.read(f) != b {
			t.Errorf("%s changed in a refused rekey", f)
		}
	}
}

func TestBoxSignerAddsAReader(t *testing.T) {
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// No box with secrets: nothing about readers.
	erin := r.key("erin")
	if out, code := r.run("signer", "KEY="+erin, "PRINCIPAL=erin"); code != 0 || strings.Contains(out, "reader") || strings.Contains(out, "age-keygen") {
		t.Fatalf("no secrets yet: exit %d\n%s", code, out)
	}
	_, box := r.boxKey("box")
	if out, code := r.secrets("", "v", "set", "APP=example", "VAR=A", "BOX_KEY="+box); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	r.commit("a secret")

	frank := r.key("frank")
	out, code := r.run("signer", "KEY="+frank, "PRINCIPAL=frank")
	if code != 0 || !strings.Contains(out, "signer: added frank to "+recipientsFile+" as a reader; the secrets already there become readable to them after a reader runs make rekey\n") {
		t.Fatalf("a reader added: exit %d\n%s", code, out)
	}
	if !strings.HasSuffix(r.read(recipientsFile), "# frank\nssh-ed25519 "+r.b64(frank)+"\n") {
		t.Errorf("recipients:\n%s", r.read(recipientsFile))
	}
	if got := r.git("diff", "--cached", "--name-only"); !strings.Contains(got, recipientsFile) {
		t.Errorf("the recipients file is not staged: %q", got)
	}
	if out, _ := r.run("signer", "KEY="+frank, "PRINCIPAL=frank"); !strings.Contains(out, recipientsFile+" already lists this key as a reader") {
		t.Errorf("again:\n%s", out)
	}

	// A key age cannot take: told what to do instead.
	priv := filepath.Join(r.home, ".ssh", "gina")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ecdsa", "-N", "", "-f", priv).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	before := r.read(recipientsFile)
	out, code = r.run("signer", "KEY="+priv+".pub", "PRINCIPAL=gina")
	if code != 0 || !strings.Contains(out, "signer: this key is an ecdsa-sha2-nistp256 key, which age cannot encrypt to, so it cannot read the boxes' secrets:") ||
		!strings.Contains(out, "make secret-show IDENTITY=~/.config/age/hotserve.txt") {
		t.Fatalf("an ecdsa key: exit %d\n%s", code, out)
	}
	if r.read(recipientsFile) != before {
		t.Error("an ecdsa key went into the recipients file")
	}

	// A file edited by hand, with no newline at its end: the key still
	// goes on a line of its own, and the edit is left for git add -p.
	r.write(recipientsFile, strings.TrimSuffix(before, "\n"))
	hank := r.key("hank")
	out, code = r.run("signer", "KEY="+hank, "PRINCIPAL=hank")
	if code != 0 || !strings.Contains(out, recipientsFile+" has other edits, or is not committed yet, so it is not staged") {
		t.Fatalf("a hand-edited file: exit %d\n%s", code, out)
	}
	if want := before + "# hank\nssh-ed25519 " + r.b64(hank) + "\n"; r.read(recipientsFile) != want {
		t.Errorf("recipients:\n%s\nwant:\n%s", r.read(recipientsFile), want)
	}
}

func TestBoxSecretsNeedAge(t *testing.T) {
	r := newBoxRepo(t) // the age CLI not on PATH
	if _, err := exec.LookPath("age"); err == nil {
		t.Skip("age is installed here")
	}
	for _, cmd := range []string{"set", "show", "rekey", "import"} {
		if out, code := r.secrets("", "A=1\n", cmd, "APP=example", "VAR=A"); code != 1 || !strings.Contains(out, "age is not installed here: brew install age, or apt install age") {
			t.Errorf("%s with no age: exit %d\n%s", cmd, code, out)
		}
	}
}

// envLines is hotserve's env-file parser (liveswap/app.go,
// parseEnvLines), copied: make secrets-import must read a file as the
// box would. Once liveswap exports it (ParseEnvFile), this test should
// call that instead of a copy.
func envLines(data string) ([]string, error) {
	var vars []string
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return nil, fmt.Errorf("line %d: not KEY=VALUE", i+1)
		}
		if !envKey.MatchString(key) {
			return nil, fmt.Errorf("line %d: %q is not a valid name", i+1, key)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		vars = append(vars, key+"="+value)
	}
	return vars, nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func TestBoxSecretsImport(t *testing.T) {
	r := secretsRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	boxID, box := r.boxKey("box")
	nbsp, em, ideo, mmsp, nel := string(rune(0xa0)), string(rune(0x2003)), string(rune(0x3000)), string(rune(0x205f)), string(rune(0x85))
	corpus := strings.Join([]string{
		"# a comment",
		"",
		"   ",
		"export A=1",
		"B = two words  ",
		`C="quoted value"`,
		`D='single'`,
		`E="unbalanced`,
		"F=a=b=c",
		"G=  spaced" + nbsp,
		em + "H=em" + em,
		`I="  inner spaces kept  "`,
		"J=#not a comment",
		"K=" + ideo + "tail" + mmsp,
		"\tTAB_ED=x\t",
		"L=" + nel + "nel" + nel,
		`M=""quoted twice""`,
		"N=caf" + string(rune(0xe9)),
		"A=again",
		"CR=line\r",
		"",
	}, "\n")
	want, err := envLines(corpus)
	if err != nil {
		t.Fatal(err)
	}
	out, code := r.secrets("", corpus, "import", "APP=example", "BOX_KEY="+box)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "secrets: note: A is given again on line 19; the last one is kept\n") {
		t.Errorf("no note of A twice:\n%s", out)
	}
	last := map[string]string{}
	for _, kv := range want {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	for k, v := range last {
		r.mustDecrypt(boxID, "deploy.example.com/secrets/example/"+k+".age", v)
	}
	if !strings.Contains(out, "secrets: deploy.example.com/secrets/example: added A B C D E F G H I J K TAB_ED L M N CR\n") ||
		!strings.Contains(out, fmt.Sprintf("secrets: %d values, encrypted to 2 keys, in the commit\n", len(last))) {
		t.Errorf("summary:\n%s", out)
	}

	// Lines hotserve refuses, it refuses, and then imports nothing.
	for _, c := range []struct{ line, want string }{
		{"1BAD=x", "line 2: the name is not letters, digits and _, not starting with a digit"},
		{"NOEQUALS", "line 2: not KEY=VALUE"},
		{"=x", "line 2: not KEY=VALUE"},
		{"export\tX=1", "line 2: the name is not letters"},
		{"BAD-NAME=x", "line 2: the name is not letters"},
		{"EMPTY=", "line 2: EMPTY is empty"},
		{`EMPTY=""`, "line 2: EMPTY is empty"},
	} {
		input := "OK=1\n" + c.line + "\n"
		if _, err := envLines(input); err == nil && !strings.Contains(c.want, "is empty") {
			t.Errorf("%q: hotserve's parser takes it, the test expects a refusal", c.line)
		}
		out, code := r.secrets("", input, "import", "APP=other")
		if code != 1 || !strings.Contains(out, "secrets: "+c.want) || !strings.Contains(out, "nothing imported") {
			t.Errorf("%q: exit %d, want %q\n%s", c.line, code, c.want, out)
		}
		if r.exists("deploy.example.com/secrets/other") {
			t.Fatalf("%q: a refused import wrote something", c.line)
		}
	}
	if out, code := r.secrets("", "# nothing\n", "import", "APP=other"); code != 1 || !strings.Contains(out, "the env file has no KEY=VALUE line") {
		t.Errorf("an empty file: exit %d\n%s", code, out)
	}
	// Two names one file on a Mac's disk, in the file or against one
	// already there.
	if out, code := r.secrets("", "foo=1\nFOO=2\n", "import", "APP=other"); code != 1 || !strings.Contains(out, "line 2: FOO and foo differ only in case") || r.exists("deploy.example.com/secrets/other") {
		t.Errorf("foo and FOO: exit %d\n%s", code, out)
	}
	if out, code := r.secrets("", "a=1\n", "import", "APP=example"); code != 1 || !strings.Contains(out, "deploy.example.com/secrets/example/A.age differs from a.age only in case") {
		t.Errorf("a against A: exit %d\n%s", code, out)
	}
	// A value at the cap, in time linear in its length.
	big := strings.Repeat("v", 65536-len("BIG")-4)
	if out, code := r.secrets("", "BIG="+big+"\n", "import", "APP=big"); code != 0 {
		t.Fatalf("a 64 KiB value: exit %d\n%s", code, out)
	}
	r.mustDecrypt(boxID, "deploy.example.com/secrets/big/BIG.age", big)
	// The wrong file entirely.
	if out, code := r.secrets("", strings.Repeat("x", 1048577), "import", "APP=other"); code != 1 || !strings.Contains(out, "the file is over 1048576 bytes") {
		t.Errorf("a file over the cap: exit %d\n%s", code, out)
	}
	// A value the box would refuse, inside a line hotserve's parser takes.
	if out, code := r.secrets("", "OK=1\nNUL=a\x00b\n", "import", "APP=other"); code != 1 || !strings.Contains(out, "NUL has a NUL byte in it") || r.exists("deploy.example.com/secrets/other") {
		t.Errorf("a NUL: exit %d\n%s", code, out)
	}
}
