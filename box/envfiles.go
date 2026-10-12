package box

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"go.uber.org/zap"

	"github.com/smallhoursorg/hotserve/box/proof"
	"github.com/smallhoursorg/hotserve/liveswap"
)

// The env files of the set (DESIGN-box.md, "Values and the env file",
// "The install transaction"): what step 15a decides root writes or
// removes under /etc/hotserve/secrets, and the reads and writes the
// transaction makes of them. The store's rules, which every function
// here keeps:
//
//  1. Root writes only `<app>.env` names in secretsDir, for an app in
//     the app grammar, and removes only one applied.json lists whose
//     app HEAD no longer has. Any other name there is left alone.
//  2. An installed env file is read without following a link, without
//     blocking, held to a regular file, to maxEnvRead plus one byte.
//     Anything at the name that is not a regular file reads as no file
//     (it is replaced, never followed); one larger than the cap is the
//     box's error before the transaction, and matches nothing during it.
//  3. A file root writes is the rendered bytes, 0640, born group
//     hotserve in the setgid directory (Paths): the unit has no
//     CAP_CHOWN. Before anything is written, the directory must be the
//     one tmpfiles.d makes, so that a file root writes is one hotserve
//     can read at the app's next launch.
//  4. Nothing here decrypts before check has passed (I6): planSecrets
//     runs after it, in the applier alone.
//  5. A value's plaintext is only in its env file and the record (I9):
//     results, journal lines and the diff carry `<app>/<VAR>` names.

// Paths of the box's key (DESIGN-box.md, "The box's key").
const (
	identityFile = "/etc/hotserve/age/identity"
	agePubFile   = "/etc/hotserve/age.pub"
)

// Read caps (DESIGN-box.md, "Caps").
const (
	// maxEnvRead caps an installed env file as root and the handler read
	// it: the rendered cap and its header. A larger one is no file root
	// wrote, and its bytes could not be kept in the record.
	maxEnvRead = 65 << 10
	// maxAgePub caps age.pub, one 62-byte recipient.
	maxAgePub = 1 << 10
)

// envNeither is the digest of an installed env file too large to be one
// root wrote: it matches no record's digest, so a transaction meeting
// one is left as found.
const envNeither = "-"

// The applier's texts for secrets, from DESIGN-box.md's Message
// catalogue; <app> is filled in by fill.
const (
	msgNoKey       = "this box has no key for secrets (/etc/hotserve/age/identity): hotserve box key, as root, makes one; then make rekey BOX_KEY=<the key it prints> in the repository, and push; nothing changed"
	msgSecretsDir  = "/etc/hotserve/secrets is not the setgid root:hotserve directory tmpfiles.d makes: systemd-tmpfiles --create hotserve-box.conf, as root, puts it back; nothing changed" //nolint:gosec // a message naming a directory, not a credential
	msgEnvTooLarge = "/etc/hotserve/secrets/<app>.env is larger than 66560 bytes, which no push writes: move it aside, as root, and push again; nothing changed"
)

// fill is a catalogue text with its app named.
func fill(msg, app string) string { return strings.Replace(msg, "<app>", app, 1) }

// failure is the box's own error with a catalogue text of its own: the
// result is `failed`, worded as msg (Transitions table, "checking →
// failed": "the key and the directory have texts of their own").
type failure struct{ msg string }

func (f *failure) Error() string { return f.msg }

func (a *Applier) secretsPath() string       { return filepath.Join(a.root, secretsDir) }
func (a *Applier) envPath(app string) string { return filepath.Join(a.secretsPath(), app+".env") }

// envTmp is the swap's temporary for app's env file, `.<app>.env.box-<id>`
// (Paths): a dot-name the launch never reads, removed by recovery.
func (a *Applier) envTmp(app, id string) string {
	return filepath.Join(a.secretsPath(), "."+app+".env.box-"+id)
}

// fileOwner is who owns what root writes in secretsDir: root, and the
// hotserve group, read off the exchange tree's base directory, which
// tmpfiles.d makes root:hotserve (Paths) — no name is looked up.
type fileOwner struct{ uid, gid uint32 }

func (a *Applier) secretsOwner() (*fileOwner, error) {
	fi, err := os.Lstat(a.x(""))
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("the exchange tree's owner cannot be read")
	}
	return &fileOwner{uid: st.Uid, gid: st.Gid}, nil
}

// checkSecretsDir holds secretsDir to what tmpfiles.d makes: a directory,
// 2750, owned as the exchange tree is. Anything else is the box's error,
// `failed` (step 15a): a file root wrote there could otherwise be one
// hotserve cannot read, which would surface at the app's next launch.
func (a *Applier) checkSecretsDir(o *fileOwner) error {
	fi, err := os.Lstat(a.secretsPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &failure{msg: msgSecretsDir}
		}
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || fi.Mode() != os.ModeDir|os.ModeSetgid|0o750 || st.Uid != o.uid || st.Gid != o.gid {
		return &failure{msg: msgSecretsDir}
	}
	return nil
}

// envFile is an installed env file as rule 2 reads it.
type envFile struct {
	present bool   // something stands at the name
	regular bool   // a regular file, read
	data    []byte // its bytes, when regular
	ours    bool   // regular, 0640 and nothing else, owned as root writes it
}

// readEnv reads what stands at app's env file (rule 2). A file larger
// than the cap is a *tooLargeError. owner, when given, decides ours.
func (a *Applier) readEnv(app string, owner *fileOwner) (envFile, error) {
	path := a.envPath(app)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0) //nolint:gosec // a fixed name in a directory only root writes
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return envFile{}, nil
	case errors.Is(err, syscall.ELOOP): // a link at the name: never followed
		return envFile{present: true}, nil
	case err != nil:
		return envFile{}, err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return envFile{}, err
	}
	if !fi.Mode().IsRegular() {
		return envFile{present: true}, nil
	}
	data, err := readCapped(f, path, maxEnvRead)
	if err != nil {
		return envFile{}, err
	}
	e := envFile{present: true, regular: true, data: data}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && owner != nil {
		e.ours = fi.Mode() == 0o640 && st.Uid == owner.uid && st.Gid == owner.gid
	}
	return e, nil
}

// envDigest is d for one env file of the set: its bytes' digest, ""
// when no regular file stands at the name, envNeither when it is too
// large to be one root wrote. An error is the read's own (I2's
// stop-as-found).
func (a *Applier) envDigest(app string) (string, error) {
	e, err := a.readEnv(app, nil)
	var big *tooLargeError
	switch {
	case errors.As(err, &big):
		return envNeither, nil
	case err != nil:
		return "", err
	case !e.regular:
		return "", nil
	}
	return digest(e.data), nil
}

// secretsPlan is step 15a's decision: the env files in the set and what
// applied.json records once the push applies.
type secretsPlan struct {
	tree      string
	entries   []envEntry        // the set's env files, in app-name order
	next      map[string][]byte // each written file's rendered bytes
	installed map[string]string // app → digest of every env file the box manages after the push
	added     []string          // <app>/<VAR>, sorted
	changed   []string
	removed   []string
	oob       []string // apps, sorted
}

// planSecrets is step 15a (DESIGN-box.md): the secrets a chain whose
// every signature verified proved (s), opened with the box's key and
// held against what stands in secretsDir and what applied.json lists
// ("Values and the env file"). caddyfile is the bundle's path. A HEAD
// with no secret reads no key, and with nothing listed either touches
// nothing. A refusal is a *refusal; the box's errors with texts of
// their own a *failure.
func (a *Applier) planSecrets(s *proof.Secrets, base *applied, caddyfile string) (*secretsPlan, error) {
	p := &secretsPlan{tree: s.Tree, next: map[string][]byte{}, installed: map[string]string{}}
	if len(s.Apps) == 0 && len(base.Secrets) == 0 {
		return p, nil
	}
	owner, err := a.secretsOwner()
	if err != nil {
		return nil, err
	}
	if err := a.checkSecretsDir(owner); err != nil {
		return nil, err
	}
	var opened []openedApp
	if len(s.Apps) > 0 {
		id, err := readIdentity(filepath.Join(a.root, identityFile))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &failure{msg: msgNoKey}
		}
		if err != nil {
			return nil, err
		}
		if opened, err = openSecrets(id, s, caddyfile); err != nil {
			return nil, err
		}
	}
	inHead := map[string]bool{}
	for _, o := range opened {
		inHead[o.name] = true
		cur, err := a.readEnvFor(o.name, owner)
		if err != nil {
			return nil, err
		}
		listed, isListed := base.Secrets[o.name]
		write := !cur.regular || !cur.ours || !bytes.Equal(cur.data, o.file)
		switch {
		case isListed && (!cur.regular || !cur.ours || digest(cur.data) != listed):
			p.oob = append(p.oob, o.name)
		case !isListed && cur.present && write:
			p.oob = append(p.oob, o.name)
		}
		p.names(o.name, cur, o.file)
		p.installed[o.name] = digest(o.file)
		if !write {
			continue
		}
		e := envEntry{App: o.name, NewSHA256: digest(o.file)}
		if cur.regular {
			e.Prev, e.PrevSHA256 = cur.data, digest(cur.data)
		}
		p.entries = append(p.entries, e)
		p.next[o.name] = o.file
	}
	for _, app := range slices.Sorted(maps.Keys(base.Secrets)) {
		if inHead[app] || !appName.MatchString(app) {
			continue
		}
		cur, err := a.readEnvFor(app, owner)
		if err != nil {
			return nil, err
		}
		if !cur.regular || digest(cur.data) != base.Secrets[app] {
			p.oob = append(p.oob, app)
		}
		if !cur.regular {
			// Nothing of the box's stands there: absent, or a link or
			// another kind the console made, which is left alone.
			continue
		}
		p.names(app, cur, nil)
		p.entries = append(p.entries, envEntry{App: app, Prev: cur.data, PrevSHA256: digest(cur.data)})
	}
	slices.SortFunc(p.entries, func(x, y envEntry) int { return strings.Compare(x.App, y.App) })
	slices.Sort(p.added)
	slices.Sort(p.changed)
	slices.Sort(p.removed)
	slices.Sort(p.oob)
	return p, nil
}

// readEnvFor is readEnv before the transaction, a file over the cap the
// catalogue's failure for it.
func (a *Applier) readEnvFor(app string, owner *fileOwner) (envFile, error) {
	cur, err := a.readEnv(app, owner)
	var big *tooLargeError
	if errors.As(err, &big) {
		return envFile{}, &failure{msg: fill(msgEnvTooLarge, app)}
	}
	return cur, err
}

// names adds to the plan the secrets app's push adds, changes and
// removes: the installed file's variables against the rendered ones
// (next nil for a removal). A file that does not read as an env file
// has none to compare.
func (p *secretsPlan) names(app string, cur envFile, next []byte) {
	was := envVars(cur.data)
	now := envVars(next)
	for _, k := range slices.Sorted(maps.Keys(now)) {
		old, ok := was[k]
		switch {
		case !ok:
			p.added = append(p.added, app+"/"+k)
		case old != now[k]:
			p.changed = append(p.changed, app+"/"+k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(was)) {
		if _, ok := now[k]; !ok {
			p.removed = append(p.removed, app+"/"+k)
		}
	}
}

// envVars is an env file's variables as a launch reads them; none for a
// file that does not parse.
func envVars(data []byte) map[string]string {
	out := map[string]string{}
	if data == nil {
		return out
	}
	vars, err := liveswap.ParseEnvLines("env file", data)
	if err != nil {
		return out
	}
	for _, kv := range vars {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// into puts the plan in the transaction: what the record carries, and
// the bytes the swap writes.
func (p *secretsPlan) into(t *txn) {
	t.rec.Secrets, t.nextEnv = p.entries, p.next
	t.rec.SecretsTree, t.rec.SecretsInstalled = p.tree, p.installed
	t.rec.SecretsAdded, t.rec.SecretsChanged, t.rec.SecretsRemoved, t.rec.SecretsOutOfBand = p.added, p.changed, p.removed, p.oob
}

// writeEnv puts one env file of the set at its new state: the rendered
// bytes renamed over the name, 0640 (rule 3), or the file removed.
func (a *Applier) writeEnv(t *txn, e envEntry) error {
	if e.NewSHA256 == "" {
		return a.removeDurable("envfile:remove", a.envPath(e.App))
	}
	return a.writeDurable("envfile:new", a.envPath(e.App), a.envTmp(e.App, t.rec.ID), t.nextEnv[e.App], 0o640)
}

// restoreEnv puts one env file back at its prev — its bytes, or no file
// — retrying once (Failure-mode table, "previous bytes back"); a write
// whose rename landed counts.
func (a *Applier) restoreEnv(t *txn, e envEntry) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if e.PrevSHA256 == "" {
			err = a.removeDurable("envfile:unlink", a.envPath(e.App))
		} else {
			err = a.writeDurable("envfile:prev", a.envPath(e.App), a.envTmp(e.App, t.rec.ID), e.Prev, 0o640)
		}
		if err == nil {
			return nil
		}
	}
	if d, derr := a.envDigest(e.App); derr == nil && d == e.PrevSHA256 {
		return nil
	}
	return err
}

// removeEnvTemps removes the env files' temporaries matching pattern in
// secretsDir: the swap's for one id, or every one a crash left. Not
// writes recovery reasons from: their bytes are never read.
func (a *Applier) removeEnvTemps(pattern string) {
	names, err := filepath.Glob(filepath.Join(a.secretsPath(), pattern))
	if err != nil {
		return
	}
	for _, p := range names {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.logger.Warn("box: could not remove a temporary", zap.String("error", proof.Bound(err.Error())))
		}
	}
}

// secretName reports whether s names a secret as results and the
// journal do: `<app>/<VAR>`, each in its grammar.
func secretName(s string) bool {
	app, v, ok := strings.Cut(s, "/")
	return ok && appName.MatchString(app) && len(v) <= proof.MaxVarName && liveswap.ValidEnvKey(v)
}
