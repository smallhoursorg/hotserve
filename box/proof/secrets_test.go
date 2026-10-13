package proof

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// testNames stands in for the box's grammars: liveswap's app names and
// the env file's variable names.
var testNames = Names{
	App: regexp.MustCompile(`^[a-z0-9-]{1,63}$`).MatchString,
	Var: regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString,
}

// secretsRepo is a commit whose tree holds deploy.example.com/Caddyfile
// and, beside it, whatever secrets entry the case puts there.
type secretsRepo struct {
	commit *Commit
	trees  map[string]*Tree
	blobs  map[string][]byte
}

// newSecretsRepo builds the commit: secrets is the host directory's
// `secrets` entry (nil for none); extra trees and blobs are bundled
// with it.
func newSecretsRepo(t *testing.T, secrets *Entry, extra ...[]byte) secretsRepo {
	t.Helper()
	r := secretsRepo{trees: map[string]*Tree{}, blobs: map[string][]byte{}}
	entries := []Entry{{ModeFile, "Caddyfile", ObjectID("blob", []byte("# the box\n"))}}
	if secrets != nil {
		entries = append(entries, *secrets)
	}
	host := r.tree(t, treeObject(entries...))
	root := r.tree(t, treeObject(Entry{ModeDir, "deploy.example.com", host.ID}))
	for _, raw := range extra {
		if _, err := ParseTree(raw); err == nil {
			r.tree(t, raw)
		} else {
			r.blobs[ObjectID("blob", raw)] = raw
		}
	}
	c, err := ParseCommit(commitObject(root.ID, nil, nil, "m\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.commit = c
	return r
}

func (r secretsRepo) tree(t *testing.T, raw []byte) *Tree {
	t.Helper()
	tr, err := ParseTree(raw)
	if err != nil {
		t.Fatal(err)
	}
	r.trees[tr.ID] = tr
	return tr
}

func (r secretsRepo) prove() (*Secrets, error) {
	return ProveSecrets(r.commit, r.trees, r.blobs, "deploy.example.com/Caddyfile", testNames)
}

func secretsEntry(raw []byte) *Entry {
	return &Entry{ModeDir, "secrets", ObjectID("tree", raw)}
}

func TestProveSecrets(t *testing.T) {
	db, stripe := []byte("age-encryption.org/v1 db"), []byte("age-encryption.org/v1 stripe")
	app := treeObject(
		Entry{ModeFile, "DATABASE_URL.age", ObjectID("blob", db)},
		Entry{ModeExecutable, "STRIPE_KEY.age", ObjectID("blob", stripe)},
	)
	other := treeObject(Entry{ModeFile, "TOKEN.age", ObjectID("blob", db)})
	secrets := treeObject(
		Entry{ModeDir, "api", ObjectID("tree", other)},
		Entry{ModeDir, "example", ObjectID("tree", app)},
		Entry{ModeFile, "recipients", ObjectID("blob", []byte("age1…\n"))},
	)

	t.Run("none", func(t *testing.T) {
		s, err := newSecretsRepo(t, nil).prove()
		if err != nil || s.Tree != "" || len(s.Apps) != 0 {
			t.Fatal(s, err)
		}
	})
	t.Run("two apps, the recipients blob not bundled", func(t *testing.T) {
		r := newSecretsRepo(t, secretsEntry(secrets), secrets, app, other, db, stripe)
		s, err := r.prove()
		if err != nil {
			t.Fatal(err)
		}
		if s.Tree != ObjectID("tree", secrets) || len(s.Apps) != 2 || s.Apps[0].Name != "api" || s.Apps[1].Name != "example" {
			t.Fatalf("%+v", s)
		}
		var names []string
		for _, v := range s.Apps[1].Vars {
			names = append(names, v.Name)
		}
		if !slices.Equal(names, []string{"DATABASE_URL", "STRIPE_KEY"}) || string(s.Apps[1].Vars[1].Data) != string(stripe) || s.Apps[1].Vars[1].Blob != ObjectID("blob", stripe) {
			t.Fatalf("%+v", s.Apps[1])
		}
	})
	t.Run("recipients alone", func(t *testing.T) {
		only := treeObject(Entry{ModeFile, "recipients", ObjectID("blob", []byte("x"))})
		s, err := newSecretsRepo(t, secretsEntry(only), only).prove()
		if err != nil || s.Tree != ObjectID("tree", only) || len(s.Apps) != 0 {
			t.Fatal(s, err)
		}
	})

	c := func(name string, raw []byte, want string, extra ...[]byte) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			r := newSecretsRepo(t, secretsEntry(raw), append([][]byte{raw}, extra...)...)
			_, err := r.prove()
			refusalContaining(t, err, strings.ReplaceAll(want, "<c>", r.commit.ID))
		})
	}
	c("a blob not bundled", secrets,
		"bundle: blob "+ObjectID("blob", stripe)+", needed for deploy.example.com/secrets/example/STRIPE_KEY.age in <c>, is not in the bundle",
		app, other, db)
	c("an app tree not bundled", secrets,
		"bundle: tree "+ObjectID("tree", other)+", needed for deploy.example.com/secrets/api in <c>, is not in the bundle",
		app, db, stripe)
	c("a file in secrets/", treeObject(Entry{ModeFile, "README", ObjectID("blob", db)}),
		"deploy.example.com/secrets/README in <c> is not an app's directory: it is a file")
	c("a symlink in secrets/", treeObject(Entry{ModeSymlink, "example", ObjectID("blob", db)}),
		"deploy.example.com/secrets/example in <c> is not an app's directory: it is a symlink")
	c("a submodule in secrets/", treeObject(Entry{ModeSubmodule, "example", ObjectID("commit", db)}),
		"deploy.example.com/secrets/example in <c> is not an app's directory: it is a submodule")
	c("recipients as a directory", treeObject(Entry{ModeDir, "recipients", ObjectID("tree", app)}),
		"deploy.example.com/secrets/recipients in <c> is not an app's directory: recipients is the name of the recipients file, which is a regular file", app, db, stripe)
	c("recipients as a symlink", treeObject(Entry{ModeSymlink, "recipients", ObjectID("blob", db)}),
		"deploy.example.com/secrets/recipients in <c> is not an app's directory: recipients is the name of the recipients file")
	c("an app name outside the grammar", treeObject(Entry{ModeDir, "Example", ObjectID("tree", app)}),
		"deploy.example.com/secrets/Example in <c> is not an app's directory: Example is not an app name", app, db, stripe)
	c("_backup, held for later", treeObject(Entry{ModeDir, "_backup", ObjectID("tree", app)}),
		"deploy.example.com/secrets/_backup in <c> is not an app's directory: _backup is not an app name", app, db, stripe)
	nested := treeObject(Entry{ModeDir, "deeper", ObjectID("tree", app)})
	c("a directory in an app's", treeObject(Entry{ModeDir, "example", ObjectID("tree", nested)}),
		"deploy.example.com/secrets/example/deeper in <c> is not a secret: it is a directory", nested, app, db, stripe)
	linked := treeObject(Entry{ModeSymlink, "K.age", ObjectID("blob", db)})
	c("a symlinked value", treeObject(Entry{ModeDir, "example", ObjectID("tree", linked)}),
		"deploy.example.com/secrets/example/K.age in <c> is not a secret: it is a symlink", linked, db)
	sub := treeObject(Entry{ModeSubmodule, "K.age", ObjectID("commit", db)})
	c("a submodule", treeObject(Entry{ModeDir, "example", ObjectID("tree", sub)}),
		"deploy.example.com/secrets/example/K.age in <c> is not a secret: it is a submodule", sub)
	plain := treeObject(Entry{ModeFile, "DATABASE_URL", ObjectID("blob", db)})
	c("a value without .age", treeObject(Entry{ModeDir, "example", ObjectID("tree", plain)}),
		"deploy.example.com/secrets/example/DATABASE_URL in <c> is not a secret: only <VAR>.age files are", plain, db)
	for _, bad := range []string{"1X.age", "my-var.age", ".age", "A B.age"} {
		named := treeObject(Entry{ModeFile, bad, ObjectID("blob", db)})
		c("a bad variable name "+bad, treeObject(Entry{ModeDir, "example", ObjectID("tree", named)}),
			"deploy.example.com/secrets/example/"+bad+" in <c> is not a secret: "+strings.TrimSuffix(bad, ".age")+" is not a variable name an env file can carry", named, db)
	}
	long := strings.Repeat("A", MaxVarName+1) + ".age"
	longTree := treeObject(Entry{ModeFile, long, ObjectID("blob", db)})
	c("a variable name over its cap", treeObject(Entry{ModeDir, "example", ObjectID("tree", longTree)}),
		"is not a secret: its name is over 128 bytes", longTree, db)
	empty := treeObject()
	c("an app with no values", treeObject(Entry{ModeDir, "example", ObjectID("tree", empty)}),
		"deploy.example.com/secrets/example in <c> is not an app's directory: it holds no secret", empty)

	t.Run("secrets as a file", func(t *testing.T) {
		r := newSecretsRepo(t, &Entry{ModeFile, "secrets", ObjectID("blob", db)})
		_, err := r.prove()
		refusalContaining(t, err, "deploy.example.com/secrets in "+r.commit.ID+" is not a directory")
	})
	t.Run("the secrets tree not bundled", func(t *testing.T) {
		r := newSecretsRepo(t, secretsEntry(secrets))
		_, err := r.prove()
		refusalContaining(t, err, "bundle: tree "+ObjectID("tree", secrets)+", needed for deploy.example.com/secrets in "+r.commit.ID+", is not in the bundle; a bin/apply from before secrets sends only the Caddyfile's trees: update it from the template")
		var ref *Refusal
		if !asRefusal(err, &ref) || !slices.Contains(ref.IDs, ObjectID("tree", secrets)) {
			t.Fatalf("the refusal does not name the tree's id: %v", ref)
		}
	})
	t.Run("a bin/apply from before secrets, with no secrets, still passes", func(t *testing.T) {
		// The trees on the Caddyfile's path are all it sends; with no
		// secrets entry they are all the proof needs.
		s, err := newSecretsRepo(t, nil).prove()
		if err != nil || s.Tree != "" {
			t.Fatal(s, err)
		}
	})
	t.Run("a blob under another id is not that blob", func(t *testing.T) {
		r := newSecretsRepo(t, secretsEntry(secrets), secrets, app, other, db, stripe)
		r.blobs[ObjectID("blob", stripe)] = db
		_, err := r.prove()
		refusalContaining(t, err, "bundle: blob "+ObjectID("blob", stripe)+", needed for deploy.example.com/secrets/example/STRIPE_KEY.age")
	})
	t.Run("caps", func(t *testing.T) {
		var vars []Entry
		var blobs [][]byte
		for i := 0; i <= MaxSecretVars; i++ {
			data := []byte{byte(i), byte(i >> 8)}
			vars = append(vars, Entry{ModeFile, "V" + string(rune('A'+i/26%26)) + string(rune('A'+i%26)) + ".age", ObjectID("blob", data)})
			blobs = append(blobs, data)
		}
		slices.SortFunc(vars, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
		vars = slices.CompactFunc(vars, func(a, b Entry) bool { return a.Name == b.Name })
		if len(vars) <= MaxSecretVars {
			t.Fatalf("only %d distinct names", len(vars))
		}
		many := treeObject(vars...)
		r := newSecretsRepo(t, secretsEntry(treeObject(Entry{ModeDir, "example", ObjectID("tree", many)})),
			append([][]byte{treeObject(Entry{ModeDir, "example", ObjectID("tree", many)}), many}, blobs...)...)
		_, err := r.prove()
		refusalContaining(t, err, "app example has 257 secrets; the cap is 256")

		one := treeObject(Entry{ModeFile, "K.age", ObjectID("blob", db)})
		var apps []Entry
		for i := 0; i <= MaxSecretApps; i++ {
			apps = append(apps, Entry{ModeDir, "app" + string(rune('a'+i/26)) + string(rune('a'+i%26)), ObjectID("tree", one)})
		}
		top := treeObject(apps...)
		r = newSecretsRepo(t, secretsEntry(top), top, one, db)
		_, err = r.prove()
		refusalContaining(t, err, "33 apps have secrets; the cap is 32")

		// Past the cap the scan only counts: a bad name it reaches there
		// is not looked at, and the count is the refusal.
		apps = apps[:0]
		for i := 0; i < 39; i++ {
			apps = append(apps, Entry{ModeDir, "app" + string(rune('a'+i/26)) + string(rune('a'+i%26)), ObjectID("tree", one)})
		}
		apps = append(apps, Entry{ModeDir, "zz_bad", ObjectID("tree", one)}) // sorts last
		top = treeObject(apps...)
		r = newSecretsRepo(t, secretsEntry(top), top, one, db)
		_, err = r.prove()
		refusalContaining(t, err, "40 apps have secrets; the cap is 32")
	})
	t.Run("a blob over its cap", func(t *testing.T) {
		big := make([]byte, MaxSecretFile+1)
		one := treeObject(Entry{ModeFile, "K.age", ObjectID("blob", big)})
		top := treeObject(Entry{ModeDir, "example", ObjectID("tree", one)})
		r := newSecretsRepo(t, secretsEntry(top), top, one, big)
		_, err := r.prove()
		refusalContaining(t, err, "deploy.example.com/secrets/example/K.age is 131073 bytes; the cap is 131072")
	})
	t.Run("a Caddyfile at the top of the repository", func(t *testing.T) {
		r := secretsRepo{trees: map[string]*Tree{}, blobs: map[string][]byte{}}
		root := r.tree(t, treeObject(Entry{ModeFile, "Caddyfile", ObjectID("blob", []byte("x"))}, *secretsEntry(secrets)))
		for _, raw := range [][]byte{secrets, app, other} {
			r.tree(t, raw)
		}
		r.blobs[ObjectID("blob", db)], r.blobs[ObjectID("blob", stripe)] = db, stripe
		c, err := ParseCommit(commitObject(root.ID, nil, nil, "m\n"))
		if err != nil {
			t.Fatal(err)
		}
		s, err := ProveSecrets(c, r.trees, r.blobs, "Caddyfile", testNames)
		if err != nil || len(s.Apps) != 2 {
			t.Fatal(s, err)
		}
	})
	t.Run("a name a refusal quotes is bounded", func(t *testing.T) {
		raw := treeObject(Entry{ModeFile, "x\x7fy", ObjectID("blob", db)})
		_, err := newSecretsRepo(t, secretsEntry(raw), raw).prove()
		refusalContaining(t, err, `deploy.example.com/secrets/"x\x7fy" in`)
	})
}
