package box

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"

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
//	warning <text>                 each cap past 75%

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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var ciphertext int64
	for _, e := range entries {
		name := e.Name()
		if name == "recipients" {
			if !e.Type().IsRegular() {
				return nil, refused("%s/recipients is not a regular file", shown)
			}
			n, err := countRecipients(filepath.Join(dir, name), shown+"/recipients")
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
	if n := len(ts.secrets.Apps); n > proof.MaxSecretApps {
		return nil, refused("%d apps have secrets; the cap is %d", n, proof.MaxSecretApps)
	} else if n >= warnSecretApps {
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return app, 0, err
	}
	var ciphertext, rendered int64
	for _, e := range entries {
		name := e.Name()
		v, isAge := strings.CutSuffix(name, ".age")
		switch {
		case !e.Type().IsRegular():
			return app, 0, refused("%s/%s is not a secret: %s", shown, proof.Bound(name), entryWhat(e))
		case !isAge:
			return app, 0, refused("%s/%s is not a secret: a secret's file is <VAR>.age", shown, proof.Bound(name))
		case len(v) > proof.MaxVarName:
			return app, 0, refused("%s/%s is not a secret: its name is over %d bytes", shown, proof.Bound(name), proof.MaxVarName)
		case !liveswap.ValidEnvKey(v):
			return app, 0, refused("%s/%s is not a secret: %s is not an environment variable's name", shown, proof.Bound(name), proof.Bound(v))
		}
		path := filepath.Join(dir, name)
		data, err := readFile(path, proof.MaxSecretFile, false)
		var big *tooLargeError
		if errors.As(err, &big) {
			fi, _ := os.Lstat(path)
			return app, 0, refused("%s/%s is %d bytes; the cap is %d", shown, name, fi.Size(), proof.MaxSecretFile)
		}
		if err != nil {
			return app, 0, err
		}
		n, err := valueLength(data)
		if err != nil {
			return app, 0, refused("%s/%s does not decrypt: %s", shown, name, err)
		}
		app.Vars = append(app.Vars, proof.SecretVar{Name: v})
		ts.sizes[app.Name+"/"+v] = n
		ciphertext += int64(len(data))
		rendered += int64(len(v)) + n + 4 // KEY="value"\n
	}
	if n := len(app.Vars); n > proof.MaxSecretVars {
		return app, 0, refused("app %s has %d secrets; the cap is %d", app.Name, n, proof.MaxSecretVars)
	} else if n >= warnSecretVars {
		ts.warnings = append(ts.warnings, fmt.Sprintf("app %s has %d secrets; the cap is %d", app.Name, n, proof.MaxSecretVars))
	}
	switch {
	case rendered > maxEnvFile:
		// A warning, not a refusal: the cap is step 15a's, and the
		// lengths are right to within a line break a value.
		ts.warnings = append(ts.warnings, fmt.Sprintf("app %s's env file would be about %d bytes, over the cap of %d: the box will refuse the push", app.Name, rendered, maxEnvFile))
	case rendered >= warnEnvFile:
		ts.warnings = append(ts.warnings, fmt.Sprintf("app %s's env file would be about %d bytes; the cap is %d", app.Name, rendered, maxEnvFile))
	}
	return app, ciphertext, nil
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

// valueLength is a binary age file's plaintext length, from its sizes
// alone: the payload after the header is a 16-byte nonce, then the
// value in 64 KiB chunks, each with a 16-byte tag, and at least one
// chunk.
func valueLength(data []byte) (int64, error) {
	if bytes.HasPrefix(data, []byte(ageArmor)) {
		return 0, errors.New("it is ASCII-armored (age -a); the box reads age's binary form")
	}
	if !bytes.HasPrefix(data, []byte(ageIntro)) {
		return 0, errors.New("it is not an age file")
	}
	header, err := age.ExtractHeader(bytes.NewReader(data))
	if err != nil {
		return 0, errors.New("its header is not one age reads")
	}
	const nonce, chunk, tag = 16, 64 << 10, 16
	q := int64(len(data)-len(header)) - nonce
	if q < tag {
		return 0, errors.New("its payload is cut short")
	}
	chunks := (q + chunk + tag - 1) / (chunk + tag)
	return q - chunks*tag, nil
}

// age1Line is a bech32 age recipient other than X25519's — a plugin's,
// age1<name>1… (age-plugin-yubikey, age-plugin-se) — whose own encoding
// describe cannot check without the plugin.
var age1Line = regexp.MustCompile(`^age1[a-z0-9]+$`)

// countRecipients reads a recipients file as age does — one recipient a
// line, `#` lines and blank ones skipped — and counts the recipients. The
// first must be the box's key, an X25519 recipient ("The box's key");
// every other an SSH key age encrypts to, or an age recipient.
func countRecipients(path, shown string) (int, error) {
	data, err := readFile(path, maxRecipientsFile, false)
	if err != nil {
		return 0, err
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxRecipientsFile)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
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
		case strings.HasPrefix(s, "ssh-"):
			_, err := agessh.ParseRecipient(s)
			ok = err == nil
		case strings.HasPrefix(s, "age1"):
			_, err := age.ParseX25519Recipient(s)
			ok = err == nil || age1Line.MatchString(s)
		}
		if !ok {
			return 0, refused("%s:%d: not a recipient age encrypts to (an ssh-ed25519 or ssh-rsa key, or an age1… recipient)", shown, line)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return n, nil
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
