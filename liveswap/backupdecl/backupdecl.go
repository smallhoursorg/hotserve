// Package backupdecl is what an app declares about its backups: which
// paths under its shared dir are SQLite databases and which are plain
// files. It is a declaration and nothing else. liveswap parses it out
// of the app's Caddyfile block and refuses a config whose declaration
// is not valid; nothing in the serving process acts on it.
//
// The package imports only the standard library, so a program that acts
// on a declaration can share this type and these rules without
// linking Caddy (TestImportsOnlyTheStandardLibrary).
package backupdecl

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// Config is one app's `backup` block. Every path is relative to the
// app's shared dir, in the spelling path.Clean produces.
type Config struct {
	// SQLite declares paths to be SQLite database files. A database's
	// -wal, -shm and -journal belong to it and are not declared.
	SQLite []string `json:"sqlite,omitempty"`

	// Files declares plain files and directories. "." is the whole
	// shared dir. A path under SQLite may sit inside one of these, and
	// is a database all the same.
	Files []string `json:"files,omitempty"`
}

// sidecars are the files SQLite keeps beside a database, named by
// appending to the database's own name. They are part of the database,
// so neither they nor anything under their names is declared.
var sidecars = []string{"-wal", "-shm", "-journal"}

type item struct{ kind, path string }

func (i item) String() string { return fmt.Sprintf("%s %q", i.kind, i.path) }

// Validate reports the first thing wrong with the declaration. The
// rules are lexical: nothing is looked up on disk, so a config can be
// checked by a user who cannot see the app's data, before the app has
// ever been deployed.
func (c *Config) Validate() error {
	if len(c.SQLite) == 0 && len(c.Files) == 0 {
		return errors.New("declares nothing: name at least one `sqlite <path>` or `files <path>`, relative to the app's shared dir")
	}
	items := make([]item, 0, len(c.SQLite)+len(c.Files))
	for _, p := range c.SQLite {
		items = append(items, item{"sqlite", p})
	}
	for _, p := range c.Files {
		items = append(items, item{"files", p})
	}
	for _, it := range items {
		if err := it.validate(); err != nil {
			return err
		}
	}
	// Every pair once. The rules are symmetric, so the order paths are
	// declared in decides nothing but which of two errors comes first.
	for i, b := range items {
		for _, a := range items[:i] {
			if err := collide(a, b); err != nil {
				return err
			}
		}
	}
	return nil
}

// validate checks one path on its own.
func (i item) validate() error {
	p := i.path
	switch {
	case p == "":
		return fmt.Errorf("%s: the path is empty", i)
	case !utf8.ValidString(p):
		return fmt.Errorf("%s: the path is not valid UTF-8, and the JSON config it is carried in cannot spell it", i)
	case strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return fmt.Errorf("%s: the path contains a control character", i)
	case strings.ContainsAny(p, "{}"):
		// Not resolved, and not resolvable: whatever reads the
		// declaration has neither a deploy in progress nor hotserve's
		// environment to resolve one with.
		return fmt.Errorf("%s: the path contains a placeholder ({ or }), and a backup path takes none; it is relative to the app's shared dir already, so `{shared_dir}/app.db` is written `app.db`", i)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%s: the path must be relative to the app's shared dir, not absolute", i)
	}
	// Outside before unclean: the clean spelling of a path that leaves
	// the shared dir is not advice worth giving.
	switch c := path.Clean(p); {
	case c == ".." || strings.HasPrefix(c, "../"):
		return fmt.Errorf("%s: the path reaches outside the app's shared dir", i)
	case c != p:
		return fmt.Errorf("%s: the path must be clean (no ., .. or doubled or trailing separators): write %q", i, c)
	case i.kind == "sqlite" && p == ".":
		return fmt.Errorf("%s: names a directory, the shared dir itself; a sqlite path names one database file", i)
	}
	return nil
}

// collide reports why a and b cannot both be declared, naming the
// path to take out. The one overlap that is meant is a database inside
// a files path.
func collide(a, b item) error {
	if a.path == b.path {
		if a.kind == b.kind {
			return fmt.Errorf("%s is declared more than once", b)
		}
		return fmt.Errorf("%q is declared as both sqlite and files; a database is declared as sqlite only", b.path)
	}
	for _, pair := range [][2]item{{a, b}, {b, a}} {
		db, other := pair[0], pair[1]
		if db.kind != "sqlite" {
			continue
		}
		for _, suffix := range sidecars {
			if side := db.path + suffix; other.path == side || within(other.path, side) {
				return fmt.Errorf("%s belongs to the database %q: its %s is part of it, not something to declare; remove that path", other, db.path, suffix)
			}
		}
		if within(other.path, db.path) {
			return fmt.Errorf("%s is under %q, which is a database, not a directory", other, db.path)
		}
	}
	if a.kind == "files" && b.kind == "files" {
		if within(b.path, a.path) {
			return fmt.Errorf("%s is already covered by %s", b, a)
		}
		if within(a.path, b.path) {
			return fmt.Errorf("%s is already covered by %s", a, b)
		}
	}
	return nil
}

// within reports whether p lies beneath dir; "." is the shared dir,
// which everything lies beneath.
func within(p, dir string) bool {
	return p != dir && (dir == "." || strings.HasPrefix(p, dir+"/"))
}
