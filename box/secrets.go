package box

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"

	"github.com/smallhoursorg/hotserve/box/proof"
	"github.com/smallhoursorg/hotserve/liveswap"
)

// The box's side of the repository's secrets (DESIGN-box.md, "Secrets"):
// its key, the opening of each value, the env file each app's values
// become, and the checks that an app's block reads them. The proof that
// the bytes are the commit's is box/proof's ProveSecrets ("Proving the
// secrets"); what root does with the files is the install transaction.

// secretsDir holds one env file per app with secrets, written by root
// and read by hotserve at each launch (DESIGN-box.md, "Paths, owners,
// and who may touch what"). The box's key is /etc/hotserve/age/identity
// ("The box's key"): root's alone, 0600 in a 0700 directory, so that no
// other account — the hotserve uid, which every app also runs as,
// included — can read it whatever its mode.
const secretsDir = "/etc/hotserve/secrets" //nolint:gosec // a directory's path, not a credential

// Caps (DESIGN-box.md, "Caps").
const (
	// maxIdentity caps the key file as read: age-keygen's format is
	// two comment lines and the key, under 200 bytes.
	maxIdentity = 4 << 10
	// maxEnvFile caps one app's env file as rendered.
	maxEnvFile = 64 << 10
)

// managedEnvFile is the env file the box writes for app: the path an
// app's block names with env_file to read its secrets.
func managedEnvFile(app string) string { return secretsDir + "/" + app + ".env" }

// secretNames are the grammars the secrets' names are held to: an app
// directory is named for a liveswap app (appName, as the token walk
// reads app lines), a value's file for a variable an env file can
// carry.
var secretNames = proof.Names{App: appName.MatchString, Var: liveswap.ValidEnvKey}

// readIdentity reads the box's key: a regular file, not followed if a
// symlink, within maxIdentity, in age-keygen's format, holding exactly
// one X25519 identity. age's own parse error is not passed on: a line
// it could not read may be the key itself.
func readIdentity(file string) (*age.X25519Identity, error) {
	data, err := readFile(file, maxIdentity, false)
	if err != nil {
		return nil, err
	}
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s is not an age key file (age-keygen's format)", file)
	}
	if len(ids) != 1 {
		return nil, fmt.Errorf("%s holds %d keys; the box's is one X25519 key", file, len(ids))
	}
	id, ok := ids[0].(*age.X25519Identity)
	if !ok {
		return nil, fmt.Errorf("%s holds a key that is not X25519; the box's is one X25519 key", file)
	}
	return id, nil
}

// ensureIdentity is the box's key, made if there is none: written whole
// to a temporary in the same directory (0600, created exclusively),
// fsynced, then linked into place — a link, not a rename, so that an
// identity already there is never replaced, by a run that raced this
// one or by anything else — and the directory fsynced. created reports
// whether this call made it. A key that is there and does not read is
// refused, never replaced: moving it aside is a person's decision,
// since every secret encrypted to it needs a rekey after.
func ensureIdentity(file string, now time.Time) (id *age.X25519Identity, created bool, err error) {
	id, err = readIdentity(file)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return id, false, err
	}
	id, err = age.GenerateX25519Identity()
	if err != nil {
		return nil, false, err
	}
	dir := filepath.Dir(file)
	tmp, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return nil, false, err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // the link holds the inode; the temporary name goes either way
	body := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n", now.UTC().Format(time.RFC3339), id.Recipient(), id)
	_, err = tmp.WriteString(body)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, false, err
	}
	if err := os.Link(tmp.Name(), file); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Another run made one first: that one is the box's key.
			id, err := readIdentity(file)
			return id, false, err
		}
		return nil, false, err
	}
	if err := os.Remove(tmp.Name()); err != nil {
		return nil, false, err
	}
	if err := syncDir(dir); err != nil {
		return nil, false, err
	}
	return id, true, nil
}

// openedApp is one app's secrets, opened: the variables' names in the
// order the commit lists them, and the env file they render to.
type openedApp struct {
	name  string
	vars  []string
	file  []byte
	shown string // <dir>/secrets/<app>, as a message names it
}

// openSecrets is step 15a (DESIGN-box.md): every value of every app
// opened with the box's key, held to the value rules, and each app's
// rendered to the env file the box will install. It runs as root, and
// only on secrets ProveSecrets proved from a chain whose every
// signature verified (I6): the bytes it decrypts are a signer's.
// caddyfile is the bundle's path, which names the directory the
// secrets sit in.
func openSecrets(id *age.X25519Identity, s *proof.Secrets, caddyfile string) ([]openedApp, error) {
	var out []openedApp
	for _, a := range s.Apps {
		app := openedApp{name: a.Name, shown: secretsShown(caddyfile) + "/" + a.Name}
		pairs := make([]string, 0, len(a.Vars))
		for _, v := range a.Vars {
			shown := app.shown + "/" + v.Name + ".age"
			plain, err := openSecret(id, v.Data, shown)
			if err != nil {
				return nil, err
			}
			value, err := secretValue(plain, shown)
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, v.Name+"="+value)
			app.vars = append(app.vars, v.Name)
		}
		file, err := renderEnvFile(a.Name, pairs)
		if err != nil {
			return nil, err
		}
		if len(file) > maxEnvFile {
			return nil, refused("%s comes to %d bytes as an env file, over the cap of %d", app.shown, len(file), maxEnvFile)
		}
		app.file = file
		out = append(out, app)
	}
	return out, nil
}

// openSecret decrypts one value with the box's key. A file encrypted to
// other keys only is refused naming the box's, so that the run says
// which key to add; one that is not an age file at all, or does not
// decrypt whole, is refused as such. Neither the plaintext nor age's
// words about it are kept beyond the value.
func openSecret(id *age.X25519Identity, data []byte, shown string) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(data), id)
	var unmatched *age.NoIdentityMatchError
	switch {
	case errors.As(err, &unmatched):
		return nil, refused("%s is not encrypted to this box's key (%s)", shown, id.Recipient())
	case err != nil:
		return nil, refused("%s is not an age file", shown)
	}
	plain, err := io.ReadAll(io.LimitReader(r, proof.MaxSecretFile+1))
	if err != nil {
		return nil, refused("%s does not decrypt whole: it was changed after it was encrypted", shown)
	}
	if len(plain) > proof.MaxSecretFile {
		return nil, refused("%s is larger than %d bytes once decrypted", shown, proof.MaxSecretFile)
	}
	return plain, nil
}

// secretValue is the value a decrypted file holds (DESIGN-box.md,
// "Values and the env file"): its bytes, less one trailing newline (LF
// or CRLF), since `echo value | age …` adds one and a value cannot hold
// one. Anything an env file cannot carry is refused: another line break,
// a CR, a NUL, bytes that are not UTF-8. An empty value is a value.
func secretValue(plain []byte, shown string) (string, error) {
	v := plain
	if s, ok := bytes.CutSuffix(v, []byte("\r\n")); ok {
		v = s
	} else if s, ok := bytes.CutSuffix(v, []byte("\n")); ok {
		v = s
	}
	switch {
	case bytes.ContainsAny(v, "\n\r"):
		return "", refused("%s holds more than one line; a value is one line", shown)
	case bytes.IndexByte(v, 0) >= 0:
		return "", refused("%s holds a NUL byte, which an environment cannot carry", shown)
	case !utf8.Valid(v):
		return "", refused("%s is not UTF-8 text", shown)
	}
	return string(v), nil
}

// renderEnvFile is an app's env file: a header naming where the values
// come from, then liveswap's rendering of them. The bytes depend only
// on the app's name and its pairs, so a rekey (new ciphertext, same
// values) renders the file the box already has.
func renderEnvFile(app string, pairs []string) ([]byte, error) {
	body, err := liveswap.RenderEnvFile(pairs)
	if err != nil {
		// secretValue and the proof's names admit only what renders.
		return nil, fmt.Errorf("rendering %s's env file: %w", app, err)
	}
	header := "# " + app + "'s secrets, written by hotserve from secrets/" + app + "/ in the box's\n" +
		"# repository. The next push that changes them replaces this file.\n"
	return append([]byte(header), body...), nil
}

// secretsShown is the secrets directory as a message names it: beside
// the Caddyfile, in the repository.
func secretsShown(caddyfile string) string {
	dir := path.Dir(caddyfile)
	if dir == "." {
		return "secrets"
	}
	return proof.Bound(dir + "/secrets")
}

// secretsFit holds the incoming file's app blocks to the secrets beside
// it (DESIGN-box.md, "Which checks run where"): every secrets directory
// names an app, every app with secrets reads its own env file and
// shadows none of them, and no app's env_file names a file in
// secretsDir the box will not write for it. These are mistakes a push
// should be told about before the app's next launch, not a boundary:
// the handler and `hotserve box describe` run them, root does not. An
// env_file's value is never quoted back, since it may hold a value the
// author meant to keep out of the run's output.
func secretsFit(shape *Shape, s *proof.Secrets, caddyfile string) error {
	dir := secretsShown(caddyfile)
	has := map[string]bool{}
	for _, a := range s.Apps {
		has[a.Name] = true
		block, ok := shape.AppEnv[a.Name]
		if !ok {
			return refused("%s/%s/ names no app in the new Caddyfile: add the app, or remove the directory", dir, a.Name)
		}
		want := managedEnvFile(a.Name)
		switch {
		case block.EnvFile == "":
			return refused("app %s has secrets, and no env_file: add `env_file %s` to its block", a.Name, want)
		case filepath.Clean(block.EnvFile) != want:
			return refused("app %s has secrets, and its env_file is not %s: set it", a.Name, want)
		}
		for _, v := range a.Vars {
			switch {
			case v.Name == "SOCKET":
				return refused("%s/%s/SOCKET.age: hotserve sets SOCKET last, so app %s would never see this value", dir, a.Name, a.Name)
			case block.hasEnv(v.Name):
				return refused("app %s sets %s with env and as a secret; env wins over env_file, so the secret would never reach it", a.Name, v.Name)
			}
		}
	}
	for _, name := range shape.Apps {
		file := shape.AppEnv[name].EnvFile
		if file == "" {
			continue
		}
		clean := filepath.Clean(file)
		rest, ok := strings.CutPrefix(clean, secretsDir+"/")
		if !ok {
			continue
		}
		other, isEnv := strings.CutSuffix(rest, ".env")
		switch {
		case isEnv && other == name:
			if !has[name] {
				return refused("app %s's env_file is %s, and %s/%s/ holds no secret: add one, or remove the env_file line", name, clean, dir, name)
			}
		case isEnv && appName.MatchString(other):
			return refused("app %s's env_file is app %s's secrets file", name, other)
		default:
			return refused("app %s's env_file is in %s, where the box keeps each app's own file: use %s", name, secretsDir, managedEnvFile(name))
		}
	}
	return nil
}
