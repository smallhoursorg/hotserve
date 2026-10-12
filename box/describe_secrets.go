package box

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/hotserve/box/proof"
	"github.com/smallhoursorg/hotserve/liveswap"
)

// `hotserve box describe`'s reading of the secrets beside a Caddyfile
// (DESIGN-box.md, "`box describe`", "Which checks run where"): the
// working tree's `<dir>/secrets/`, held to the layout table as root holds
// the commit's, without a key. Each value's length comes from its
// ciphertext alone — age pads nothing — to within the line break step
// 15a strips; the caps past 75% are warnings, so a pull request's check
// run shows a cliff before a push meets it.
//
// The lines it adds to describe's, in this order, for bin/check to read
// (each value held to a grammar, so none can break a line):
//
//	recipients <n>                 the recipients file's recipient lines, when there is one
//	secret <app> <VAR> <bytes>     each secret, apps and variables in git's order
//	warning <text>                 each cap past 75%, and each dot-name, which is not read

// treeSecrets is a working tree's secrets as describe reads them.
type treeSecrets struct {
	secrets    *proof.Secrets   // names only: what secretsFit reads
	sizes      map[string]int64 // <app>/<VAR> → the value's length in bytes
	recipients int              // recipient lines, -1 when there is no recipients file
	warnings   []string
}

// Thresholds of the 75% warnings (DESIGN-box.md, Caps).
const (
	warnSecretApps = proof.MaxSecretApps * 3 / 4
	warnSecretVars = proof.MaxSecretVars * 3 / 4
	warnEnvFile    = maxEnvFile * 3 / 4
	warnCiphertext = proof.MaxBundle * 3 / 4
	// maxRecipientsFile bounds the recipients file as describe reads it.
	maxRecipientsFile = 1 << 20
)

// readTreeSecrets reads the secrets beside the Caddyfile at caddyfile, a
// path in the working tree. No `secrets` beside it is none.
func readTreeSecrets(caddyfile string) (*treeSecrets, error) {
	dir := filepath.Join(filepath.Dir(caddyfile), "secrets")
	shown := secretsShown(caddyfile)
	ts := &treeSecrets{secrets: &proof.Secrets{}, sizes: map[string]int64{}, recipients: -1}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ts, nil
	case err != nil:
		return nil, err
	case !fi.IsDir():
		return nil, refused("%s is not a directory", shown)
	}
	entries, err := gitOrder(dir)
	if err != nil {
		return nil, err
	}
	// One pass before any secret's file is opened, as the commit's proof
	// scans its tree: in git's order, each entry held to the layout, the
	// apps counted — past the cap only counted — and the cap refused
	// last, so the first refusal is the one root would name.
	apps := 0
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, "."):
			continue
		case name == "recipients":
			if !e.Type().IsRegular() {
				return nil, refused("%s/recipients is not a regular file", shown)
			}
			continue
		case !e.IsDir() || e.Type()&fs.ModeSymlink != 0:
			return nil, refused("%s/%s is not an app's directory: %s", shown, proof.Bound(name), entryWhat(e))
		}
		inner, err := os.ReadDir(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(inner, func(f fs.DirEntry) bool { return !strings.HasPrefix(f.Name(), ".") }) {
			continue // git tracks no empty directory, nor usually a dot-name
		}
		if apps++; apps > proof.MaxSecretApps {
			continue
		}
		if !appName.MatchString(name) {
			return nil, refused("%s/%s is not an app's directory: %s is not an app name", shown, proof.Bound(name), proof.Bound(name))
		}
	}
	if apps > proof.MaxSecretApps {
		return nil, refused("%d apps have secrets; the cap is %d", apps, proof.MaxSecretApps)
	}
	var ciphertext int64
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			ts.warnings = append(ts.warnings, notCommitted(shown+"/"+proof.Bound(name)))
			continue
		}
		if name == "recipients" {
			if !e.Type().IsRegular() {
				return nil, refused("%s/recipients is not a regular file", shown)
			}
			n, err := countRecipients(filepath.Join(dir, name), shown+"/recipients", ts)
			if err != nil {
				return nil, err
			}
			ts.recipients = n
			continue
		}
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			return nil, refused("%s/%s is not an app's directory: %s", shown, proof.Bound(name), entryWhat(e))
		}
		if !appName.MatchString(name) {
			return nil, refused("%s/%s is not an app's directory: %s is not an app name", shown, proof.Bound(name), proof.Bound(name))
		}
		app, n, err := readTreeApp(filepath.Join(dir, name), shown+"/"+name, ts)
		if err != nil {
			return nil, err
		}
		ciphertext += n
		if len(app.Vars) == 0 {
			continue // git tracks no empty directory: the commit has none
		}
		ts.secrets.Apps = append(ts.secrets.Apps, app)
	}
	if n := len(ts.secrets.Apps); n >= warnSecretApps {
		ts.warnings = append(ts.warnings, fmt.Sprintf("%d apps have secrets; the cap is %d", n, proof.MaxSecretApps))
	}
	if ciphertext >= warnCiphertext {
		ts.warnings = append(ts.warnings, fmt.Sprintf("%s holds %d bytes of ciphertext; a push's bundle is capped at %d", shown, ciphertext, proof.MaxBundle))
	}
	return ts, nil
}

// readTreeApp reads one app's directory: its secrets' names and each
// value's length, and returns the ciphertext's bytes.
func readTreeApp(dir, shown string, ts *treeSecrets) (proof.SecretApp, int64, error) {
	app := proof.SecretApp{Name: filepath.Base(dir)}
	entries, err := gitOrder(dir)
	if err != nil {
		return app, 0, err
	}
	// One pass before any file is opened, as the commit's proof scans an
	// app's tree: in git's order, each entry held to the layout up to
	// the cap, the rest only counted, and the cap refused last.
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if count++; count > proof.MaxSecretVars {
			continue
		}
		if err := secretEntry(e, shown); err != nil {
			return app, 0, err
		}
	}
	if count > proof.MaxSecretVars {
		return app, 0, refused("app %s has %d secrets; the cap is %d", app.Name, count, proof.MaxSecretVars)
	}
	var ciphertext, rendered int64
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			ts.warnings = append(ts.warnings, notCommitted(shown+"/"+proof.Bound(name)))
			continue
		}
		v := strings.TrimSuffix(name, ".age") // secretEntry held it to <VAR>.age
		size, n, err := secretFileLength(filepath.Join(dir, name))
		var big *tooLargeError
		var notAge *notAgeError
		switch {
		case errors.As(err, &big):
			return app, 0, refused("%s/%s is %d bytes; the cap is %d", shown, name, size, proof.MaxSecretFile)
		case errors.As(err, &notAge):
			return app, 0, refused("%s/%s does not decrypt: %s", shown, name, notAge.why)
		case err != nil:
			return app, 0, err
		}
		app.Vars = append(app.Vars, proof.SecretVar{Name: v})
		ts.sizes[app.Name+"/"+v] = n
		ciphertext += size
		rendered += int64(len(v)) + n + 4 // KEY="value"\n
	}
	if n := len(app.Vars); n >= warnSecretVars {
		ts.warnings = append(ts.warnings, fmt.Sprintf("app %s has %d secrets; the cap is %d", app.Name, n, proof.MaxSecretVars))
	}
	// A warning, not a refusal: the cap is step 15a's, and each value's
	// length counts a line break (up to 2 bytes) that 15a may strip, so
	// the box surely refuses only when the file is over even without them.
	switch least := rendered - 2*int64(len(app.Vars)); {
	case least > maxEnvFile:
		ts.warnings = append(ts.warnings, fmt.Sprintf("app %s's env file would be about %d bytes, over the cap of %d: the box will refuse the push", app.Name, rendered, maxEnvFile))
	case rendered > maxEnvFile:
		ts.warnings = append(ts.warnings, fmt.Sprintf("app %s's env file would be about %d bytes, at or just over the cap of %d: the box refuses the push unless values end in the line break it strips", app.Name, rendered, maxEnvFile))
	case rendered >= warnEnvFile:
		ts.warnings = append(ts.warnings, fmt.Sprintf("app %s's env file would be about %d bytes; the cap is %d", app.Name, rendered, maxEnvFile))
	}
	return app, ciphertext, nil
}

// secretEntry holds one entry of an app's directory to the layout: a
// regular file `<VAR>.age`, VAR an env file's name within its cap.
func secretEntry(e fs.DirEntry, shown string) error {
	name := e.Name()
	v, isAge := strings.CutSuffix(name, ".age")
	switch {
	case !e.Type().IsRegular():
		return refused("%s/%s is not a secret: %s", shown, proof.Bound(name), entryWhat(e))
	case !isAge:
		return refused("%s/%s is not a secret: a secret's file is <VAR>.age", shown, proof.Bound(name))
	case len(v) > proof.MaxVarName:
		return refused("%s/%s is not a secret: its name is over %d bytes", shown, proof.Bound(name), proof.MaxVarName)
	case !liveswap.ValidEnvKey(v):
		return refused("%s/%s is not a secret: %s is not an environment variable's name", shown, proof.Bound(name), proof.Bound(v))
	}
	return nil
}

// entryWhat names what a directory entry is, for a refusal.
func entryWhat(e fs.DirEntry) string {
	switch t := e.Type(); {
	case t&fs.ModeSymlink != 0:
		return "a link"
	case t.IsDir():
		return "a directory"
	case t.IsRegular():
		return "a file"
	}
	return "not a regular file"
}

// gitOrder lists dir as a git tree orders it: by name, a directory's
// name read as if it ended in `/` — so `api-v2` comes before `api` —
// and so in the order root walks the commit's.
func gitOrder(dir string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	key := func(e fs.DirEntry) string {
		if e.IsDir() {
			return e.Name() + "/"
		}
		return e.Name()
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(key(a), key(b)) })
	return entries, nil
}

// notCommitted is the warning for a dot-name in the working tree's
// secrets — `.DS_Store`, an editor's swap file — which git usually
// ignores: not read, since the commit may not have it, but the box
// refuses it if it does.
func notCommitted(path string) string {
	return path + " is not a secret, and is not read here; the box refuses the push if it is committed"
}

// notAgeError is why a secret's file is not an age file the box reads,
// in the words of root's refusal for it.
type notAgeError struct{ why string }

func (e *notAgeError) Error() string { return e.why }

// secretFileLength is a secret's file's size and its value's length,
// read as far as the header: the file is never read whole. A file over
// the cap is a *tooLargeError with its size.
func secretFileLength(path string) (size, n int64, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0) //nolint:gosec // the operator's own working tree
	if err != nil {
		return 0, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, 0, fmt.Errorf("%s: %w", path, errNotRegular)
	}
	if size = fi.Size(); size > proof.MaxSecretFile {
		return size, 0, &tooLargeError{name: path, limit: proof.MaxSecretFile}
	}
	n, err = valueLengthOf(f, size)
	return size, n, err
}

// valueLength is valueLengthOf over bytes in memory.
func valueLength(data []byte) (int64, error) {
	return valueLengthOf(bytes.NewReader(data), int64(len(data)))
}

// valueLengthOf is a binary age file's plaintext length, from its header
// and its size alone: the payload after the header is a 16-byte nonce,
// then the value in 64 KiB chunks, each with a 16-byte tag, and at
// least one chunk. Why it is not one the box reads is a *notAgeError.
func valueLengthOf(r io.ReaderAt, size int64) (int64, error) {
	notAge := func(why string) error { return &notAgeError{why: why} }
	head := make([]byte, len(ageArmor))
	got, _ := r.ReadAt(head, 0)
	head = head[:got]
	if bytes.HasPrefix(head, []byte(ageArmor)) {
		return 0, notAge("it is ASCII-armored (age -a); the box reads age's binary form")
	}
	if !bytes.HasPrefix(head, []byte(ageIntro)) {
		return 0, notAge("it is not an age file")
	}
	header, err := age.ExtractHeader(io.NewSectionReader(r, 0, size))
	if err != nil {
		return 0, notAge("its header is not one age reads")
	}
	// Every chunk but the last is full; the last is never empty unless
	// it is the only one (age's STREAM). So after the full frames comes
	// either nothing, or a frame with a tag and at least one byte: a
	// remainder of 1 to 16 bytes is a frame cut short.
	const nonce, chunk, tag = 16, 64 << 10, 16
	const frame = chunk + tag
	q := size - int64(len(header)) - nonce
	full, rest := q/frame, q%frame
	switch {
	case q == tag: // the empty value: one empty chunk
		return 0, nil
	case q < tag, rest > 0 && rest <= tag:
		return 0, notAge("its payload is cut short")
	case rest == 0:
		return full * chunk, nil
	}
	return full*chunk + rest - tag, nil
}

// agePluginLine is a plugin's age recipient, age1<name>1<data>
// (age-plugin-yubikey, age-plugin-se, age1pq…): a second `1` after the
// name, then bech32 data, which has no `1` of its own. Its checksum is
// the plugin's to check; an X25519 recipient (one `1`) that does not
// parse is a mistyped key, never a plugin's.
var agePluginLine = regexp.MustCompile(`^age1[a-z0-9]+1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]+$`)

// countRecipients reads a recipients file as age does — one recipient a
// line, read as it is (no space trimmed), a line that is empty or starts
// with `#` skipped — and counts the recipients. There must be one, and
// the first must be the box's key, an X25519 recipient ("The box's
// key"); every other an SSH key age encrypts to, or an age recipient.
func countRecipients(path, shown string, ts *treeSecrets) (int, error) {
	data, err := readFile(path, maxRecipientsFile, false)
	if err != nil {
		return 0, err
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxRecipientsFile)
	for line := 1; sc.Scan(); line++ {
		s := sc.Text()
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		// A line's CR is the scanner's to drop, as age's own scanner does;
		// a space is the recipient's, which age then cannot parse.
		if s != strings.TrimSpace(s) {
			return 0, refused("%s:%d: a space or tab at the line's start or end, which age reads as part of the recipient", shown, line)
		}
		if n == 0 {
			if _, err := age.ParseX25519Recipient(s); err != nil {
				return 0, refused("%s:%d: the first recipient is not the box's key, an age1… X25519 recipient (hotserve box key prints it)", shown, line)
			}
			n++
			continue
		}
		var ok bool
		switch {
		case strings.HasPrefix(s, "age1"):
			_, err := age.ParseX25519Recipient(s)
			ok = err == nil || agePluginLine.MatchString(s)
		default:
			_, err := agessh.ParseRecipient(s)
			ok = err == nil
			if !ok && unsupportedSSHKey(s) {
				// As age does: skipped, with a warning, never an error.
				ts.warnings = append(ts.warnings, fmt.Sprintf("%s:%d: age skips this %s key (it encrypts only to ssh-ed25519 and ssh-rsa): its holder cannot read the secrets; give them an age1… recipient instead", shown, line, sshKeyType(s)))
				continue
			}
		}
		if !ok {
			return 0, refused("%s:%d: not a recipient age encrypts to (an ssh-ed25519 or ssh-rsa key, or an age1… recipient)", shown, line)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, refused("%s has no recipient: its first is the box's key (hotserve box key prints it)", shown)
	}
	return n, nil
}

// unsupportedSSHKey reports whether s is an SSH public key that age reads
// but does not encrypt to — an sk- key, an ECDSA one — which age skips
// in a recipients file with a warning.
func unsupportedSSHKey(s string) bool {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s))
	return err == nil && k.Type() != ssh.KeyAlgoED25519 && k.Type() != ssh.KeyAlgoRSA
}

// sshKeyType is s's key type, the line's first field.
func sshKeyType(s string) string {
	t, _, _ := strings.Cut(s, " ")
	return proof.Bound(t)
}

// lines are describe's lines for the secrets.
func (ts *treeSecrets) lines() []string {
	var out []string
	if ts.recipients >= 0 {
		out = append(out, fmt.Sprintf("recipients %d", ts.recipients))
	}
	for _, a := range ts.secrets.Apps {
		for _, v := range a.Vars {
			out = append(out, fmt.Sprintf("secret %s %s %d", a.Name, v.Name, ts.sizes[a.Name+"/"+v.Name]))
		}
	}
	for _, w := range ts.warnings {
		out = append(out, "warning "+w)
	}
	return out
}
