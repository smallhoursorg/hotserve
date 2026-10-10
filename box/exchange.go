package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// Where the box keeps its state (DESIGN-box.md, "Paths, owners, and
// who may touch what"). The handler reads applied.json, which `init`
// and the applier write (PR 3, PR 4) with the type below, so that the
// reader and the writers cannot disagree about a field. The markers
// and results a result poll reads come with the applier.
const (
	installedFile = "/etc/hotserve/Caddyfile"
	exchangeDir   = "/var/lib/hotserve-box"
)

// maxApplied caps applied.json (DESIGN-box.md, "Caps"): its path is up
// to 4 KiB, which JSON's escapes can grow sixfold.
const maxApplied = 64 << 10

// applied is applied.json, the baseline: the commit the box runs.
type applied struct {
	SHA    string    `json:"sha"`
	Path   string    `json:"path"`
	SHA256 string    `json:"sha256"`
	Signer string    `json:"signer"`
	When   time.Time `json:"when"`
}

var errNotRegular = errors.New("not a regular file")

// readFile reads one file whole, at most limit bytes: opened without
// blocking (a FIFO standing at the name cannot hang the request) and,
// unless follow, without following a symlink in the last component;
// held to a regular file by the descriptor it opened; read through a
// reader that stops one byte past the cap, since a stat is not a
// bound. A missing file is fs.ErrNotExist, for the caller to tell
// from the rest.
func readFile(path string, limit int64, follow bool) ([]byte, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if !follow {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0) //nolint:gosec // a fixed path of the box's
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", path, errNotRegular)
	}
	return readCapped(f, path, limit)
}

// readCapped reads r to its end, at most limit bytes: through a reader
// that stops one byte past the cap, so more is refused, never cut.
func readCapped(r io.Reader, name string, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", name, limit)
	}
	return b, nil
}

// readApplied reads applied.json, refusing a symlink at its name (the
// exchange tree's directories come from tmpfiles.d, in a base
// directory the hotserve uid cannot write).
func readApplied(dir string) (*applied, error) {
	path := filepath.Join(dir, "applied.json")
	b, err := readFile(path, maxApplied, false)
	if err != nil {
		return nil, err
	}
	var a applied
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !proof.IsID(a.SHA) {
		return nil, fmt.Errorf("%s: sha is not a 40-hex commit id", path)
	}
	return &a, nil
}
