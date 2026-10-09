package proof

import (
	"strings"
)

// Caps on the path a bundle names (DESIGN-box.md, "Store rules").
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
	cur := c.Tree
	for i, name := range parts {
		t, ok := trees[cur]
		if !ok || t.ID != cur {
			return refuse("bundle: tree %s, needed for %s in %s, is not in the bundle", cur, shown, c.ID)
		}
		e, ok := t.entry(name)
		if !ok {
			return refuse("the file sent is not %s in %s", shown, c.ID)
		}
		if i < len(parts)-1 {
			if e.Mode != ModeDir {
				return refuse("the file sent is not %s in %s", shown, c.ID)
			}
			cur = e.ID
			continue
		}
		switch e.Mode {
		case ModeFile, ModeExecutable:
		default:
			return refuse("%s in %s is not a regular file", shown, c.ID)
		}
		if ObjectID("blob", file) != e.ID {
			return refuse("the file sent is not %s in %s", shown, c.ID)
		}
	}
	return nil
}
