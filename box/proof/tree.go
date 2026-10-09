package proof

import (
	"bytes"
	"encoding/hex"
)

// MaxTree bounds one tree object (DESIGN-box.md, "Store rules").
const MaxTree = 1 << 20

// Tree modes git writes. Anything else is not an entry git made.
const (
	ModeFile       = "100644"
	ModeExecutable = "100755"
	ModeSymlink    = "120000"
	ModeSubmodule  = "160000"
	ModeDir        = "40000"
)

// Entry is one line of a tree: a mode, a name and the id it points at.
type Entry struct {
	Mode string
	Name string
	ID   string
}

// Tree is a parsed tree object; ID is computed from Raw.
type Tree struct {
	Raw     []byte
	ID      string
	Entries []Entry
}

// ParseTree reads a tree object: `<mode> <name>\0<20-byte id>`
// repeated, at most MaxTree bytes. It refuses a mode git does not
// write, a name that is empty, `.`, `..` or contains `/`, a truncated
// entry, and a name that appears twice — the walk (ProveFile) must
// never have to choose between two entries of one name.
func ParseTree(raw []byte) (*Tree, error) {
	if len(raw) > MaxTree {
		return nil, refuse("bundle: a tree object is larger than 1 MiB")
	}
	t := &Tree{Raw: raw, ID: ObjectID("tree", raw)}
	seen := map[string]bool{}
	rest := raw
	for len(rest) > 0 {
		mode, after, ok := bytes.Cut(rest, []byte(" "))
		if !ok {
			return nil, refuse("bundle: tree %s: a truncated entry", t.ID)
		}
		switch string(mode) {
		case ModeFile, ModeExecutable, ModeSymlink, ModeSubmodule, ModeDir:
		default:
			return nil, refuse("bundle: tree %s: an entry mode git does not write", t.ID)
		}
		name, after, ok := bytes.Cut(after, []byte{0})
		if !ok || len(after) < 20 {
			return nil, refuse("bundle: tree %s: a truncated entry", t.ID)
		}
		n := string(name)
		if n == "" || n == "." || n == ".." || bytes.IndexByte(name, '/') >= 0 {
			return nil, refuse("bundle: tree %s: an entry name that is not one path component", t.ID)
		}
		if seen[n] {
			return nil, refuse("bundle: tree %s: an entry name appears twice", t.ID)
		}
		seen[n] = true
		t.Entries = append(t.Entries, Entry{Mode: string(mode), Name: n, ID: hex.EncodeToString(after[:20])})
		rest = after[20:]
	}
	return t, nil
}

// entry is the tree's entry of that name, if any.
func (t *Tree) entry(name string) (Entry, bool) {
	for _, e := range t.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}
