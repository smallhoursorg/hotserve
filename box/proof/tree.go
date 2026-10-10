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

// Tree is a parsed tree object: the raw bytes, checked to be well
// formed, and the id computed from them. Entries are not materialised
// — a bundle may carry a mebibyte of them per tree, and the proof
// looks up one name per tree on the path — so a lookup scans the bytes
// and Entries builds the slice only when asked.
type Tree struct {
	Raw []byte
	ID  string
}

// ParseTree reads a tree object: `<mode> <name>\0<20-byte id>`
// repeated, at most MaxTree bytes. It refuses a mode git does not
// write, a name that is empty, `.`, `..` or contains `/`, and a
// truncated entry. It allocates nothing per entry; a name that appears
// twice is refused when it is looked up (entry), since the walk must
// never choose between two entries of one name.
func ParseTree(raw []byte) (*Tree, error) {
	if len(raw) > MaxTree {
		return nil, refuse("bundle: a tree object is larger than 1 MiB")
	}
	t := &Tree{Raw: raw, ID: ObjectID("tree", raw)}
	err := t.scan(func(Entry) bool { return true })
	if err != nil {
		return nil, err
	}
	return t, nil
}

// scan walks the entries, calling visit for each (a false return stops
// the walk), and refuses a malformed one. Entry values handed to visit
// alias nothing: Mode and Name are converted, ID is hex-encoded.
func (t *Tree) scan(visit func(Entry) bool) error {
	rest := t.Raw
	for len(rest) > 0 {
		mode, after, ok := bytes.Cut(rest, []byte(" "))
		if !ok {
			return refuse("bundle: tree %s: a truncated entry", t.ID)
		}
		switch string(mode) {
		case ModeFile, ModeExecutable, ModeSymlink, ModeSubmodule, ModeDir:
		default:
			return refuse("bundle: tree %s: an entry mode git does not write", t.ID)
		}
		name, after, ok := bytes.Cut(after, []byte{0})
		if !ok || len(after) < 20 {
			return refuse("bundle: tree %s: a truncated entry", t.ID)
		}
		if len(name) == 0 || string(name) == "." || string(name) == ".." || bytes.IndexByte(name, '/') >= 0 {
			return refuse("bundle: tree %s: an entry name that is not one path component", t.ID)
		}
		rest = after[20:]
		if !visit(Entry{Mode: string(mode), Name: string(name), ID: hex.EncodeToString(after[:20])}) {
			return nil
		}
	}
	return nil
}

// entry is the tree's entry of that name, if any; a name that appears
// twice is refused.
func (t *Tree) entry(name string) (Entry, bool, error) {
	var found Entry
	n := 0
	err := t.scan(func(e Entry) bool {
		if e.Name == name {
			found = e
			n++
		}
		return n < 2
	})
	switch {
	case err != nil:
		return Entry{}, false, err
	case n > 1:
		return Entry{}, false, refuse("bundle: tree %s: an entry name appears twice", t.ID)
	case n == 1:
		return found, true, nil
	}
	return Entry{}, false, nil
}

// Entries materialises every entry, in order. For tests and tools; the
// proof never needs them all.
func (t *Tree) Entries() ([]Entry, error) {
	var out []Entry
	err := t.scan(func(e Entry) bool {
		out = append(out, e)
		return true
	})
	return out, err
}
