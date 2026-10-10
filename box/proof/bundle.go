package proof

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Caps on a bundle (DESIGN-box.md, "Store rules").
const (
	MaxBundle    = 16 << 20 // decompressed; the gzip reader stops here
	MaxCaddyfile = 1 << 20
)

// Bundle is one push, parsed from memory under strict rules: regular
// files only, fixed names — `path`, `Caddyfile`, `commit`,
// `parents/NNNN`, `trees/<sha>` — and nothing else. Parents are in
// the workflow's order, `parents/0001` first, a sequence with no gap;
// Chain checks that each is the first parent of the one before. Trees
// are keyed by the id they are filed under, which the object must hash
// to. The same parser runs in the handler (step 3) and in the applier
// (step 9).
type Bundle struct {
	// Path is the file's path in the repository, as the workflow knows
	// it: a relative path of safe components (SplitPath).
	Path      string
	Caddyfile []byte
	// Commit is HEAD, the commit the token's sha claim must name.
	Commit  *Commit
	Parents []*Commit
	Trees   map[string]*Tree
}

// ReadBundle parses a gzip tarball. The decompressed stream is read
// through a limit of MaxBundle plus one byte and refused if it gets
// there; each entry has its own cap, checked from the header before a
// byte of it is read.
func ReadBundle(gz []byte) (*Bundle, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, refuse("bundle: not a gzip stream")
	}
	lr := &io.LimitedReader{R: zr, N: MaxBundle + 1}
	tr := tar.NewReader(lr)
	b := &Bundle{Trees: map[string]*Tree{}}
	seen := map[string]bool{}
	parents := map[int]*Commit{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if lr.N == 0 {
				return nil, refuse("bundle: larger than 16 MiB")
			}
			return nil, refuse("bundle: not a tar stream")
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, refuse("bundle: %s is not a regular file", Bound(hdr.Name))
		}
		name := hdr.Name
		if seen[name] {
			return nil, refuse("bundle: %s appears twice", Bound(name))
		}
		seen[name] = true
		limit, ok := entryCap(name)
		if !ok {
			return nil, refuse("bundle: %s is not a bundle file", Bound(name))
		}
		if hdr.Size < 0 || hdr.Size > int64(limit) {
			return nil, refuse("bundle: %s is larger than its cap of %d bytes", Bound(name), limit)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			if lr.N == 0 {
				return nil, refuse("bundle: larger than 16 MiB")
			}
			return nil, refuse("bundle: not a tar stream")
		}
		switch {
		case name == "path":
			// One trailing newline is forgiven: `echo` adds one and
			// the path is a one-line file by any reading.
			p := strings.TrimSuffix(string(data), "\n")
			if _, err := SplitPath(p); err != nil {
				return nil, err
			}
			b.Path = p
		case name == "Caddyfile":
			b.Caddyfile = data
		case name == "commit":
			c, err := ParseCommit(data)
			if err != nil {
				return nil, err
			}
			b.Commit = c
		case strings.HasPrefix(name, "parents/"):
			// HEAD plus its bundled parents is the chain above the
			// baseline, at most MaxChain commits: MaxChain-1 parents.
			n, _ := strconv.Atoi(name[len("parents/"):]) // entryCap checked the four digits
			if n == 0 || n > MaxChain-1 {
				return nil, refuse("bundle: %s is outside parents/0001 to parents/%04d", Bound(name), MaxChain-1)
			}
			c, err := ParseCommit(data)
			if err != nil {
				return nil, err
			}
			parents[n] = c
		case strings.HasPrefix(name, "trees/"):
			// The proof walks at most MaxDepth trees, so a bundle has no
			// use for more; the cap keeps a flood of tiny valid trees
			// from costing a map entry each.
			if len(b.Trees) == MaxDepth {
				return nil, refuse("bundle: more than %d trees", MaxDepth)
			}
			id := name[len("trees/"):]
			t, err := ParseTree(data)
			if err != nil {
				return nil, err
			}
			if t.ID != id {
				return nil, refuse("bundle: %s does not hash to its name", Bound(name))
			}
			b.Trees[id] = t
		}
	}
	// The tar's end markers are not the stream's end: what follows —
	// a tar writer's padding, or anything else — is read through the
	// same limit, so the cap holds for the whole decompressed stream
	// and the gzip trailer (its checksum) is reached and checked.
	if _, err := io.Copy(io.Discard, lr); err != nil {
		return nil, refuse("bundle: not a gzip stream")
	}
	if lr.N == 0 {
		return nil, refuse("bundle: larger than 16 MiB")
	}
	for _, want := range []string{"path", "Caddyfile", "commit"} {
		if !seen[want] {
			return nil, refuse("bundle: no %s file", want)
		}
	}
	if len(b.Caddyfile) == 0 {
		return nil, refuse("bundle: Caddyfile is empty")
	}
	// parents/NNNN is a sequence 0001..N: the chain as the workflow
	// listed it, with nothing missing from the middle.
	for n := 1; n <= len(parents); n++ {
		c, ok := parents[n]
		if !ok {
			return nil, refuse("bundle: parents/ is not a sequence from 0001")
		}
		b.Parents = append(b.Parents, c)
	}
	return b, nil
}

// entryCap is the size cap for a bundle file of that name, or false
// for a name the format does not allow. Names are exact: no leading
// `./`, no directory entries, no fifth file.
func entryCap(name string) (int, bool) {
	switch {
	case name == "path":
		return MaxPath + 1, true // plus the forgiven newline
	case name == "Caddyfile":
		return MaxCaddyfile, true
	case name == "commit":
		return MaxCommit, true
	case strings.HasPrefix(name, "parents/"):
		rest := name[len("parents/"):]
		if len(rest) != 4 {
			return 0, false
		}
		for i := 0; i < 4; i++ {
			if rest[i] < '0' || rest[i] > '9' {
				return 0, false
			}
		}
		return MaxCommit, true
	case strings.HasPrefix(name, "trees/"):
		if !IsID(name[len("trees/"):]) {
			return 0, false
		}
		return MaxTree, true
	}
	return 0, false
}

// String is a one-line description for logs: the commit, the path and
// what was bundled.
func (b *Bundle) String() string {
	return fmt.Sprintf("commit %s, %s, %d parents, %d trees", b.Commit.ID, Bound(b.Path), len(b.Parents), len(b.Trees))
}
