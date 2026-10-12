package box

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// `GET /`'s recipient is age.pub's one line: omitted when there is none,
// and when it does not read as one X25519 recipient — journaled, never a
// 500 — and held out of the response filter, which would mask a bech32
// key.
func TestHandlerStatusRecipient(t *testing.T) {
	r := newRig(t)
	r.applied(t, sha)
	key := newIdentity(t).Recipient().String()
	for name, c := range map[string]struct {
		pub  []byte // nil: no file
		want string
		log  bool
	}{
		"no key":             {},
		"the key":            {pub: []byte(key + "\n"), want: key},
		"no newline":         {pub: []byte(key), log: true},
		"two lines":          {pub: []byte(key + "\n" + key + "\n"), log: true},
		"not a recipient":    {pub: []byte("age1nope\n"), log: true},
		"an SSH key instead": {pub: []byte("ssh-ed25519 AAAA\n"), log: true},
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(r.h.agePub)
			if c.pub != nil {
				r.write(t, r.h.agePub, c.pub)
			}
			before := len(r.logs.FilterMessage("box webhook could not read its state").All())
			w := r.do(t, req{target: "/", token: r.token(t)})
			got := body(t, w)
			if w.Code != http.StatusOK {
				t.Fatalf("%d %v", w.Code, got)
			}
			if rec, ok := got["recipient"]; c.want == "" && ok || c.want != "" && rec != c.want {
				t.Errorf("recipient %v, want %q", got["recipient"], c.want)
			}
			if logged := len(r.logs.FilterMessage("box webhook could not read its state").All()) > before; logged != c.log {
				t.Errorf("journaled %v, want %v", logged, c.log)
			}
		})
	}
}

// readsSecrets is boxFile whose app example reads its secrets.
func readsSecrets(version int, signers ...testKey) []byte {
	return bytes.Replace(boxFile(version, signers...), []byte("\t\t\tcommand node server.js\n"),
		[]byte("\t\t\tcommand node server.js\n\t\t\tenv_file /etc/hotserve/secrets/example.env\n"), 1)
}

// A push whose secrets and Caddyfile disagree is refused at admission
// (step 6, last), 422, nothing dropped; one that agrees is admitted.
func TestPushSecretsFit(t *testing.T) {
	p := newPushRig(t)
	val := encrypt(t, []byte("v"), newIdentity(t).Recipient())
	c := p.repo.commitSecrets(boxFile(2, p.alice), secrets{"example": {"KEY": val}}, &p.alice, p.base)
	id := randomID(t)
	m := wantResult(t, p.post(t, p.h, push{id: id, token: p.token(t, c), body: bytes.NewReader(tgz(t, p.repo.bundleFiles(c, p.base)))}), http.StatusUnprocessableEntity, id, phaseRefused)
	if m["error"] != "app example has secrets and no env_file: add env_file /etc/hotserve/secrets/example.env to its block" {
		t.Fatalf("%v", m)
	}
	p.untouched(t)

	c = p.repo.commitSecrets(readsSecrets(2, p.alice), secrets{"example": {"KEY": val}}, &p.alice, p.base)
	id = randomID(t)
	if w := p.post(t, p.h, push{id: id, token: p.token(t, c), body: bytes.NewReader(tgz(t, p.repo.bundleFiles(c, p.base)))}); w.Code != http.StatusGatewayTimeout || !exists(p.x("in/"+id+".tar")) {
		t.Fatalf("a push whose secrets fit: %d %s", w.Code, w.Body)
	}
}

// The fast path answers a replay of the commit the box runs only while
// every env file it lists stands at its digest: after the loss of one,
// the push goes to root, which writes it again.
func TestPushFastPathSecrets(t *testing.T) {
	p := newPushRig(t)
	p.secretsDirAs(0o750 | os.ModeSetgid)
	id, _, err := ensureIdentity(p.path(identityFile), p.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	c := p.repo.commitSecrets(readsSecrets(2, p.alice), secrets{"example": {"KEY": encrypt(t, []byte("v"), id.Recipient())}}, &p.alice, p.base)
	if r := p.pushOne(p.repo.bundleFiles(c, p.base)); r.Phase != phaseApplied {
		t.Fatalf("setup: %+v", r)
	}
	replay := tgz(t, p.repo.bundleFiles(c, p.base)) // HEAD's parent is the base: no parents/
	rid := randomID(t)
	wantResult(t, p.post(t, p.h, push{id: rid, token: p.token(t, c), body: bytes.NewReader(replay)}), http.StatusOK, rid, phaseNoChange)

	if err := os.Remove(p.envPath("example")); err != nil {
		t.Fatal(err)
	}
	rid = randomID(t)
	if w := p.post(t, p.h, push{id: rid, token: p.token(t, c), body: bytes.NewReader(replay)}); w.Code != http.StatusGatewayTimeout || !exists(p.x("in/"+rid+".tar")) {
		t.Fatalf("a replay over a lost env file: %d %s", w.Code, w.Body)
	}
	must(t, p.run(hooks{}))
	if r := p.result(rid); r == nil || r.Phase != phaseApplied || p.env("example") == nil {
		t.Fatalf("root did not write the env file again: %+v", r)
	}
}

// A result's secrets' names reach the workflow as root wrote them: a
// variable's name can look as random as a key, and the filter would
// mask it.
func TestResultSecretNamesUnmasked(t *testing.T) {
	p := newPushRig(t)
	name := "example/K8XQ2PL7ZR4MW9TB3NV6YC1HD5JF0GSA"
	id := randomID(t)
	p.writeResult(t, result{ID: id, Phase: phaseApplied, Commit: p.base, SecretsAdded: []string{name}})
	w := p.get(t, "/?result="+id, p.token(t, ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), name) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// writeTree makes a box directory in a working tree: its Caddyfile and
// the given files under secrets/ (a nil value is a directory).
func writeTree(t *testing.T, caddyfile []byte, files map[string][]byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "deploy.example.com")
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), caddyfile, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		p := filepath.Join(dir, "secrets", name)
		if data == nil {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "Caddyfile")
}

// describe reads the working tree's secrets as root reads the commit's:
// each value's length from its ciphertext, the recipients counted, the
// layout and the consistency checks refused, the caps past 75% warned.
func TestDescribeSecrets(t *testing.T) {
	box := newIdentity(t)
	reader := newIdentity(t)
	enc := func(v string) []byte { return encrypt(t, []byte(v), box.Recipient(), reader.Recipient()) }
	reads := secretsFile("\t\t\tenv_file /etc/hotserve/secrets/example.env\n", "")
	recipients := []byte("# the box\n" + box.Recipient().String() + "\n# alice\n" + reader.Recipient().String() + "\n")

	path := writeTree(t, reads, map[string][]byte{
		"recipients":               recipients,
		"example/DATABASE_URL.age": enc("postgres://db"),
		"example/EMPTY.age":        enc(""),
		"example/BIG.age":          enc(strings.Repeat("x", 40000)),
		"example/NL.age":           enc("value\n"),
	})
	var out bytes.Buffer
	if err := describe(path, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"recipients 2\n",
		"secret example BIG 40000\n",
		"secret example DATABASE_URL 13\n",
		"secret example EMPTY 0\n",
		"secret example NL 6\n", // the line break 15a strips is still counted: "to within"
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "warning") {
		t.Errorf("a warning under every cap:\n%s", out.String())
	}

	for name, c := range map[string]struct {
		file  []byte
		files map[string][]byte
		want  string
	}{
		"a directory for no app":           {reads, map[string][]byte{"other/KEY.age": enc("v")}, "secrets/other/ names no app in the new Caddyfile"},
		"secrets the block does not read":  {secretsFile("", ""), map[string][]byte{"example/KEY.age": enc("v")}, "app example has secrets and no env_file"},
		"a file that is not a secret":      {reads, map[string][]byte{"example/KEY": []byte("plain")}, "example/KEY is not a secret: a secret's file is <VAR>.age"},
		"a variable name env cannot carry": {reads, map[string][]byte{"example/1KEY.age": enc("v")}, "1KEY is not an environment variable's name"},
		"an app name out of grammar":       {reads, map[string][]byte{"Example/KEY.age": enc("v")}, "secrets/Example is not an app's directory"},
		"a stray file":                     {reads, map[string][]byte{"notes.txt": []byte("x")}, "secrets/notes.txt is not an app's directory: a file"},
		"an armored value":                 {reads, map[string][]byte{"example/KEY.age": []byte("-----BEGIN AGE ENCRYPTED FILE-----\nx\n")}, "is ASCII-armored"},
		"a plaintext value":                {reads, map[string][]byte{"example/KEY.age": []byte("hunter2\n")}, "does not decrypt: it is not an age file"},
		"recipients without the box first": {reads, map[string][]byte{"recipients": []byte("ssh-ed25519 AAAA\n"), "example/KEY.age": enc("v")}, "recipients:1: the first recipient is not the box's key"},
		"a recipient age cannot read":      {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + "\nssh-ed25519 AAAA\n"), "example/KEY.age": enc("v")}, "recipients:2: not a recipient age encrypts to"},
		"a mistyped reader's key":          {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + "\n" + mistyped(reader.Recipient().String()) + "\n"), "example/KEY.age": enc("v")}, "recipients:2: not a recipient age encrypts to"},
		"a trailing space":                 {reads, map[string][]byte{"recipients": []byte(box.Recipient().String() + " \n"), "example/KEY.age": enc("v")}, "recipients:1: a space or tab at the line's start or end"},
		"a recipients file with none":      {reads, map[string][]byte{"recipients": []byte("# the box\n"), "example/KEY.age": enc("v")}, "secrets/recipients has no recipient"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeTree(t, c.file, c.files)
			var out bytes.Buffer
			err := describe(path, &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want it to contain %q", err, c.want)
			}
			if out.Len() != 0 {
				t.Errorf("a refusal printed %q", out.String())
			}
		})
	}

	// Past the env file's cap is said plainly, still as a warning.
	path = writeTree(t, reads, map[string][]byte{"example/BIG.age": enc(strings.Repeat("x", 70000))})
	out.Reset()
	if err := describe(path, &out); err != nil || !strings.Contains(out.String(), "warning app example's env file would be about 70007 bytes, over the cap of 65536: the box will refuse the push\n") {
		t.Errorf("over the cap: %v\n%s", err, out.String())
	}

	// A plugin's recipient is taken as it is: its checksum is the
	// plugin's to check.
	path = writeTree(t, reads, map[string][]byte{
		"recipients":      []byte(box.Recipient().String() + "\nage1yubikey1qwt50d05nh5vutpdzmlg5wn80xq5negm4uj9ghv0snvdd3yysf5yw3rhl3t\n"),
		"example/KEY.age": enc("v"),
	})
	if err := describe(path, &bytes.Buffer{}); err != nil {
		t.Errorf("a plugin's recipient: %v", err)
	}

	// As age reads a recipients file: a CRLF line is a line, and an SSH
	// key of a type age does not encrypt to is skipped with a warning.
	sk := "sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIGYd6dqGEgHa0E1ijwY+XaYHV1Ts9ux0iEsNx8K1DtwPAAAABHNzaDo= alice"
	path = writeTree(t, reads, map[string][]byte{
		"recipients":      []byte(box.Recipient().String() + "\r\n" + sk + "\r\n"),
		"example/KEY.age": enc("v"),
	})
	out.Reset()
	if err := describe(path, &out); err != nil || !strings.Contains(out.String(), "recipients 1\n") || !strings.Contains(out.String(), "recipients:2: age skips this sk-ssh-ed25519@openssh.com key") {
		t.Errorf("CRLF and an sk- key: %v\n%s", err, out.String())
	}

	// A dot-name, which git usually ignores, is warned about and not read.
	path = writeTree(t, reads, map[string][]byte{".DS_Store": []byte("x"), "example/.KEY.age.swp": []byte("x"), "example/KEY.age": enc("v")})
	out.Reset()
	if err := describe(path, &out); err != nil || strings.Count(out.String(), "is not a secret, and is not read here; the box refuses the push if it is committed\n") != 2 {
		t.Errorf("dot-names: %v\n%s", err, out.String())
	}

	// A directory with no secret is not a commit's: git keeps none.
	path = writeTree(t, secretsFile("", ""), map[string][]byte{"example": nil})
	if err := describe(path, &bytes.Buffer{}); err != nil {
		t.Errorf("an empty app directory: %v", err)
	}
}

// mistyped is an age recipient with one data character changed, so its
// checksum fails.
func mistyped(r string) string {
	c := byte('q')
	if r[10] == 'q' {
		c = 'p'
	}
	return r[:10] + string(c) + r[11:]
}

// describe lists the working tree in git's order: a directory sorts as
// if its name ended in `/`, so api-v2 comes before api, as root walks the
// commit's tree.
func TestGitOrder(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"api", "api-v2"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "recipients"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := gitOrder(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want := "api-v2 api recipients"; strings.Join(got, " ") != want {
		t.Errorf("%v, want %s", got, want)
	}
}

// The caps past 75% are warnings in describe's output, before a push
// meets them.
func TestDescribeSecretsWarns(t *testing.T) {
	id := newIdentity(t)
	files := map[string][]byte{}
	one := encrypt(t, []byte("v"), id.Recipient())
	for i := 0; i < warnSecretVars; i++ {
		files["example/K"+strings.Repeat("A", i%5)+itoa(i)+".age"] = one
	}
	files["example/BIGGEST.age"] = encrypt(t, bytes.Repeat([]byte("x"), warnEnvFile), id.Recipient())
	path := writeTree(t, secretsFile("\t\t\tenv_file /etc/hotserve/secrets/example.env\n", ""), files)
	var out bytes.Buffer
	if err := describe(path, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"warning app example has 193 secrets; the cap is 256\n",
		"warning app example's env file would be about ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return digits[i : i+1]
	}
	return itoa(i/10) + digits[i%10:i%10+1]
}

// valueLength is the plaintext's length from an age file's sizes alone,
// for every chunk count.
func TestValueLength(t *testing.T) {
	id := newIdentity(t)
	for _, n := range []int{0, 1, 64<<10 - 1, 64 << 10, 64<<10 + 1, 2 * 64 << 10, 3*64<<10 + 7} {
		data := encrypt(t, bytes.Repeat([]byte("a"), n), id.Recipient())
		got, err := valueLength(data)
		if err != nil || got != int64(n) {
			t.Errorf("%d bytes: got %d, %v", n, got, err)
		}
	}
	var two age.Recipient = newIdentity(t).Recipient()
	if got, err := valueLength(encrypt(t, []byte("abc"), id.Recipient(), two)); err != nil || got != 3 {
		t.Errorf("two recipients: %d %v", got, err)
	}
	// A file cut inside its last frame — 1 to 16 bytes past a full one,
	// so no room for a tag and a byte — is refused, as root's decrypt
	// would be; one cut at a frame's end reads as the shorter value it
	// then is (which root's decrypt also refuses: the last chunk is not
	// marked final — describe reads sizes, not frames).
	whole := encrypt(t, bytes.Repeat([]byte("a"), 70000), id.Recipient())
	header, err := age.ExtractHeader(bytes.NewReader(whole))
	if err != nil {
		t.Fatal(err)
	}
	afterFull := len(header) + 16 + 64<<10 + 16
	for cut := 1; cut <= 16; cut++ {
		if _, err := valueLength(whole[:afterFull+cut]); err == nil {
			t.Errorf("cut %d bytes into the last frame: accepted", cut)
		}
	}
	if _, err := valueLength(whole[:len(header)+15]); err == nil {
		t.Error("a payload shorter than a nonce and a tag: accepted")
	}
}

// describe refuses a count past its cap before it reads a secret's file:
// the commit's proof refuses as early.
func TestDescribeSecretsCapsBeforeReading(t *testing.T) {
	files := map[string][]byte{}
	for i := 0; i <= proof.MaxSecretVars; i++ {
		files["example/K"+itoa(i)+".age"] = []byte("not read") // a read would refuse it as not an age file
	}
	path := writeTree(t, secretsFile("\t\t\tenv_file /etc/hotserve/secrets/example.env\n", ""), files)
	if err := describe(path, &bytes.Buffer{}); err == nil || err.Error() != "app example has 257 secrets; the cap is 256" {
		t.Errorf("%v", err)
	}
	files = map[string][]byte{}
	for i := 0; i <= proof.MaxSecretApps; i++ {
		files["app"+itoa(i)+"/KEY.age"] = []byte("not read")
	}
	if err := describe(writeTree(t, secretsFile("", ""), files), &bytes.Buffer{}); err == nil || err.Error() != "33 apps have secrets; the cap is 32" {
		t.Errorf("%v", err)
	}
}
