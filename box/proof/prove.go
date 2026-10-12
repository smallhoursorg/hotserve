package proof

import (
	"encoding/hex"
	"slices"
	"strings"
)

// Caps on the path a bundle names (DESIGN-box.md, "Caps").
const (
	MaxPath  = 4096 // bytes
	MaxDepth = 32   // components
)

// SplitPath is the repository path's components, if it is a relative
// path of safe ones: at most MaxPath bytes and MaxDepth components,
// each non-empty, neither `.` nor `..`, with no control byte (NUL and
// newline included). The path is the workflow's claim of where the
// file sits in the commit; it is walked through the bundled trees and
// never touches a filesystem.
func SplitPath(path string) ([]string, error) {
	bad := refuse("bundle: path is not a relative path of safe components")
	if path == "" || len(path) > MaxPath || strings.ContainsFunc(path, isControl) {
		return nil, bad
	}
	parts := strings.Split(path, "/")
	if len(parts) > MaxDepth {
		return nil, bad
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, bad
		}
	}
	return parts, nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// ProveFile is step 13: the file is the committed file. The commit's
// root tree must be among the bundled trees and hash to the id the
// commit names; the path's components are walked through the trees;
// the last entry must be a blob (ModeFile or ModeExecutable) whose id
// is sha1("blob <n>\0" + file). A symlink, a submodule or a tree at the
// path is refused as not a regular file; a path that is not in the
// commit, or a file that is not the committed one, as "the file sent
// is not <path> in <sha>". Trees are keyed by id; Bundle guarantees a
// tree hashes to the id it is filed under.
func ProveFile(c *Commit, trees map[string]*Tree, path string, file []byte) error {
	parts, err := SplitPath(path)
	if err != nil {
		return err
	}
	shown := Bound(path)
	notHere := refuse("the file sent is not %s in %s", shown, oid(c.ID))
	t, err := walkTree(c, trees, parts[:len(parts)-1], shown, notHere)
	if err != nil {
		return err
	}
	e, ok, err := t.entry(parts[len(parts)-1])
	if err != nil {
		return err
	}
	if !ok {
		return notHere
	}
	switch e.Mode {
	case ModeFile, ModeExecutable:
	default:
		return refuse("%s in %s is not a regular file", shown, oid(c.ID))
	}
	if ObjectID("blob", file) != e.ID {
		return notHere
	}
	return nil
}

// walkTree walks dirs — directory names, from the top of the commit —
// through the bundled trees and returns the tree at the end: the
// commit's root tree for none. A tree the walk needs and the bundle
// lacks is refused by id, naming shown as what it was needed for; a
// name the tree does not have, or has as anything but a directory, is
// absent.
func walkTree(c *Commit, trees map[string]*Tree, dirs []string, shown string, absent error) (*Tree, error) {
	t, err := bundledTree(c, trees, c.Tree, shown)
	if err != nil {
		return nil, err
	}
	for _, name := range dirs {
		e, ok, err := t.entry(name)
		if err != nil {
			return nil, err
		}
		if !ok || e.Mode != ModeDir {
			return nil, absent
		}
		if t, err = bundledTree(c, trees, e.ID, shown); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// bundledTree is the bundled tree of that id, or the refusal that names
// it as missing.
func bundledTree(c *Commit, trees map[string]*Tree, id, shown string) (*Tree, error) {
	t, ok := trees[id]
	if !ok || t.ID != id {
		return nil, refuse("bundle: tree %s, needed for %s in %s, is not in the bundle", oid(id), shown, oid(c.ID))
	}
	return t, nil
}

// Caps on a commit's secrets (DESIGN-box.md, "Caps" and "Proving the
// secrets").
const (
	MaxSecretApps = 32        // apps with a directory under secrets/
	MaxSecretVars = 256       // <VAR>.age files in one app's directory
	MaxVarName    = 128       // bytes in a VAR
	MaxSecretFile = 128 << 10 // one <VAR>.age file, encrypted
)

// Secrets is what a commit holds under secrets/ beside its Caddyfile,
// proven: every name in it, and every encrypted value as the bytes of
// the blob the commit's trees name.
type Secrets struct {
	// Tree is the secrets tree's id, or "" when the commit has none.
	Tree string
	// Apps are the app directories, in the tree's order (git's: by name).
	Apps []SecretApp
}

// SecretApp is one app's directory under secrets/.
type SecretApp struct {
	Name string
	Vars []SecretVar
}

// SecretVar is one <VAR>.age file: the variable's name, and its blob,
// still encrypted.
type SecretVar struct {
	Name string
	Blob string
	Data []byte
}

// Names are the grammars ProveSecrets holds directory and file names
// to. They are the box's to give — an app name is liveswap's, a
// variable name is what an env file can carry — and this package
// imports neither.
type Names struct {
	App func(string) bool
	Var func(string) bool
}

// ProveSecrets reads the secrets beside the Caddyfile at path from the
// commit's trees (DESIGN-box.md, "Proving the secrets"): the entry
// `secrets` in the Caddyfile's directory. No entry is no secrets; one
// that is not a directory is refused. In it, the one file is
// `recipients` — for the laptop: the box never reads it, and its blob
// need not be bundled — and every other entry is a directory named for
// an app; in an app's directory, every entry is a regular file
// `<VAR>.age`. Anything else — a symlink, a submodule, a directory one
// level too deep, a name outside the grammars — is refused by name.
// Every <VAR>.age blob must be bundled, so that what the commit holds
// is what the box sees: a file the tree no longer lists is a deletion
// the commit proves, and a blob the bundle lacks is refused, never read
// as one. ProveFile must have passed for the same path.
func ProveSecrets(c *Commit, trees map[string]*Tree, blobs map[string][]byte, path string, names Names) (*Secrets, error) {
	parts, err := SplitPath(path)
	if err != nil {
		return nil, err
	}
	dir := parts[:len(parts)-1]
	shownDir := Bound(strings.Join(append(slices.Clone(dir), "secrets"), "/"))
	dt, err := walkTree(c, trees, dir, Bound(path), refuse("the file sent is not %s in %s", Bound(path), oid(c.ID)))
	if err != nil {
		return nil, err
	}
	e, ok, err := dt.entry("secrets")
	if err != nil {
		return nil, err
	}
	if !ok {
		return &Secrets{}, nil
	}
	if e.Mode != ModeDir {
		return nil, refuse("%s in %s is not a directory", shownDir, oid(c.ID))
	}
	// Every box repository has its own copy of the template's bin/apply,
	// which an upgrade of the box does not touch; one from before
	// secrets sends only the trees on the Caddyfile's path, and the
	// refusal says so.
	st, ok := trees[e.ID]
	if !ok || st.ID != e.ID {
		return nil, refuse("bundle: tree %s, needed for %s in %s, is not in the bundle; a bin/apply from before secrets sends only the Caddyfile's trees: update it from the template", oid(e.ID), shownDir, oid(c.ID))
	}
	// One scan over the tree's bytes, as everywhere in the proof: an
	// entry's name becomes a string only once it is held to the layout,
	// and only up to the cap, past which the scan just counts.
	type ref struct{ name, id string }
	var apps []ref
	n := 0
	var bad error
	err = st.scan(func(mode, name, id []byte) bool {
		if string(name) == "recipients" {
			if m := string(mode); m != ModeFile && m != ModeExecutable {
				bad = refuse("%s/recipients in %s is not an app's directory: recipients is the name of the recipients file, which is a regular file", shownDir, oid(c.ID))
				return false
			}
			return true
		}
		if string(mode) != ModeDir {
			bad = refuse("%s/%s in %s is not an app's directory: %s", shownDir, Bound(string(name)), oid(c.ID), modeWhat(string(mode)))
			return false
		}
		if n++; n > MaxSecretApps {
			return true
		}
		a := string(name)
		if !names.App(a) {
			bad = refuse("%s/%s in %s is not an app's directory: %s is not an app name", shownDir, Bound(a), oid(c.ID), Bound(a))
			return false
		}
		apps = append(apps, ref{a, hex.EncodeToString(id)})
		return true
	})
	switch {
	case err != nil:
		return nil, err
	case bad != nil:
		return nil, bad
	case n > MaxSecretApps:
		return nil, refuse("%d apps have secrets; the cap is %d", n, MaxSecretApps)
	}
	out := &Secrets{Tree: st.ID}
	for _, a := range apps {
		app, err := proveSecretApp(c, trees, blobs, a.name, a.id, shownDir+"/"+a.name, names)
		if err != nil {
			return nil, err
		}
		out.Apps = append(out.Apps, *app)
	}
	return out, nil
}

// proveSecretApp is one app's directory: its <VAR>.age files, each a
// bundled blob.
func proveSecretApp(c *Commit, trees map[string]*Tree, blobs map[string][]byte, name, id, shown string, names Names) (*SecretApp, error) {
	t, err := bundledTree(c, trees, id, shown)
	if err != nil {
		return nil, err
	}
	type ref struct{ name, id, shown string }
	var vars []ref
	n := 0
	var bad error
	err = t.scan(func(mode, file, id []byte) bool {
		if m := string(mode); m != ModeFile && m != ModeExecutable {
			bad = refuse("%s/%s in %s is not a secret: %s", shown, Bound(string(file)), oid(c.ID), modeWhat(m))
			return false
		}
		if n++; n > MaxSecretVars {
			return true
		}
		f := string(file)
		where := shown + "/" + Bound(f)
		v, ok := strings.CutSuffix(f, ".age")
		switch {
		case !ok:
			bad = refuse("%s in %s is not a secret: only <VAR>.age files are", where, oid(c.ID))
		case len(v) > MaxVarName:
			bad = refuse("%s in %s is not a secret: its name is over %d bytes", where, oid(c.ID), MaxVarName)
		case !names.Var(v):
			bad = refuse("%s in %s is not a secret: %s is not a variable name an env file can carry", where, oid(c.ID), Bound(v))
		default:
			vars = append(vars, ref{v, hex.EncodeToString(id), where})
			return true
		}
		return false
	})
	switch {
	case err != nil:
		return nil, err
	case bad != nil:
		return nil, bad
	case n > MaxSecretVars:
		return nil, refuse("app %s has %d secrets; the cap is %d", name, n, MaxSecretVars)
	case n == 0:
		return nil, refuse("%s in %s is not an app's directory: it holds no secret", shown, oid(c.ID))
	}
	app := &SecretApp{Name: name}
	for _, v := range vars {
		data, ok := blobs[v.id]
		if !ok || ObjectID("blob", data) != v.id {
			return nil, refuse("bundle: blob %s, needed for %s in %s, is not in the bundle", oid(v.id), v.shown, oid(c.ID))
		}
		if len(data) > MaxSecretFile {
			return nil, refuse("%s is %d bytes; the cap is %d", v.shown, len(data), MaxSecretFile)
		}
		app.Vars = append(app.Vars, SecretVar{Name: v.name, Blob: v.id, Data: data})
	}
	return app, nil
}

// modeWhat names what an entry is that the layout did not expect there.
func modeWhat(mode string) string {
	switch mode {
	case ModeFile, ModeExecutable:
		return "it is a file"
	case ModeSymlink:
		return "it is a symlink"
	case ModeSubmodule:
		return "it is a submodule"
	}
	return "it is a directory"
}
