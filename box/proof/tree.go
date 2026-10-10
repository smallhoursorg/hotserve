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
// looks up one name per tree on the path — so validation and lookup
// scan the bytes, and Entries builds the slice only when asked.
type Tree struct {
	Raw []byte
	ID  string
}

// ParseTree reads a tree object: `<mode> <name>\0<20-byte id>`
// repeated, at most MaxTree bytes. It refuses a mode git does not
// write, a name that is empty, `.`, `..` or contains `/`, a truncated
// entry, entries out of git's order — names ascending, a directory
// compared as if it ended in `/` — and a name that appears twice,
// which git's fsck finds two ways: adjacent equal names, and a file
// `x` with a directory `x` later on, however many `x-…` names lie
// between them (`x` < `x-y` < `x/`). The scan converts nothing: it
// compares byte slices, and the stack it keeps for the second rule
// holds slices of Raw.
func ParseTree(raw []byte) (*Tree, error) {
	if len(raw) > MaxTree {
		return nil, refuse("bundle: a tree object is larger than 1 MiB")
	}
	t := &Tree{Raw: raw, ID: ObjectID("tree", raw)}
	var prevName []byte
	prevDir, first := false, true
	var order error
	// cands is git's df_dup_candidates: files whose directory twin
	// could still follow. A file is pushed; it is popped once an entry
	// sorts past where its twin would stand.
	cands := make([][]byte, 0, 4)
	err := t.scan(func(mode, name, _ []byte) bool {
		dir := string(mode) == ModeDir
		if !first {
			switch {
			case bytes.Equal(prevName, name):
				order = refuse("bundle: tree %s: an entry name appears twice", t.ID)
				return false
			case compareTreeNames(prevName, prevDir, name, dir) > 0:
				order = refuse("bundle: tree %s: entries are not in git's order", t.ID)
				return false
			}
		}
		for len(cands) > 0 && compareTreeNames(cands[len(cands)-1], true, name, dir) < 0 {
			cands = cands[:len(cands)-1]
		}
		if dir && len(cands) > 0 && bytes.Equal(cands[len(cands)-1], name) {
			order = refuse("bundle: tree %s: an entry name appears twice", t.ID)
			return false
		}
		if !dir {
			cands = append(cands, name)
		}
		first, prevName, prevDir = false, name, dir
		return true
	})
	if err != nil {
		return nil, err
	}
	if order != nil {
		return nil, order
	}
	return t, nil
}

// compareTreeNames is git's base_name_compare: bytes in order, and a
// directory's name read as though it ended in `/`, so that the file
// `a`, then the file `a-b` ('-' is 0x2d), then the directory `a` (read
// as `a/`, 0x2f), then the file `a0`.
func compareTreeNames(a []byte, aDir bool, b []byte, bDir bool) int {
	n := min(len(a), len(b))
	if c := bytes.Compare(a[:n], b[:n]); c != 0 {
		return c
	}
	next := func(s []byte, dir bool) int {
		if len(s) > n {
			return int(s[n])
		}
		if dir {
			return '/'
		}
		return 0
	}
	return next(a, aDir) - next(b, bDir)
}

// scan walks the entries as byte slices, calling visit for each (a
// false return stops the walk), and refuses a malformed one. Nothing
// is allocated per entry; the slices alias Raw.
func (t *Tree) scan(visit func(mode, name, id []byte) bool) error {
	rest := t.Raw
	for len(rest) > 0 {
		mode, after, ok := bytes.Cut(rest, []byte(" "))
		if !ok {
			return refuse("bundle: tree %s: a truncated entry", t.ID)
		}
		switch string(mode) { // a comparison, not a conversion: the compiler does not allocate for it
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
		if !visit(mode, name, after[:20]) {
			return nil
		}
	}
	return nil
}

// entry is the tree's entry of that name, if any — one at most, since
// ParseTree held the tree to git's order.
func (t *Tree) entry(name string) (Entry, bool, error) {
	var found Entry
	ok := false
	err := t.scan(func(mode, n, id []byte) bool {
		if string(n) != name {
			return true
		}
		found, ok = Entry{Mode: string(mode), Name: string(n), ID: hex.EncodeToString(id)}, true
		return false
	})
	return found, ok, err
}

// Entries materialises every entry, in order. For tests and tools; the
// proof never needs them all.
func (t *Tree) Entries() ([]Entry, error) {
	var out []Entry
	err := t.scan(func(mode, name, id []byte) bool {
		out = append(out, Entry{Mode: string(mode), Name: string(name), ID: hex.EncodeToString(id)})
		return true
	})
	return out, err
}
