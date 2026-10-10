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
// who may touch what"). The handler reads applied.json, `out/` and
// `stage/*.auth`; the applier and `init` write them (PR 3, PR 4), with
// the types below, so that the reader and the writers cannot disagree
// about a field.
const (
	installedFile = "/etc/hotserve/Caddyfile"
	exchangeDir   = "/var/lib/hotserve-box"
)

// Caps on the files the handler reads (DESIGN-box.md, "Caps").
const (
	maxApplied = 16 << 10 // its path is up to 4 KiB
	maxMarker  = 4 << 10
	maxResult  = 2 << 20 // a 64 KiB diff, and apps bounded only by the 1 MiB file they came from
)

// pendingLife is how long after its marker's `posted` a push is
// pending and its poll secret honoured (DESIGN-box.md, "Caps").
const pendingLife = 15 * time.Minute

// applied is applied.json, the baseline: the commit the box runs.
type applied struct {
	SHA    string    `json:"sha"`
	Path   string    `json:"path"`
	SHA256 string    `json:"sha256"`
	Signer string    `json:"signer"`
	When   time.Time `json:"when"`
}

// marker is `stage/<id>.auth`: the handler's note that the push with
// that id was admitted, holding the digest of its poll secret.
type marker struct {
	SHA256 string    `json:"sha256"` // hex sha256 of the poll secret's 32 raw bytes
	Posted time.Time `json:"posted"`
}

// result is `out/<id>.json`: what a push came to (DESIGN-box.md,
// "Record and result fields").
type result struct {
	ID              string   `json:"id"`
	Commit          string   `json:"commit,omitempty"`
	Path            string   `json:"path,omitempty"`
	Signer          string   `json:"signer,omitempty"`
	Phase           string   `json:"phase"`
	Error           string   `json:"error,omitempty"`
	Diff            string   `json:"diff,omitempty"`
	Apps            []string `json:"apps,omitempty"`
	BoxWebhook      string   `json:"box_webhook,omitempty"`
	EditedOutOfBand bool     `json:"caddyfile_edited_out_of_band"`
}

// phaseStatus is the status a result answers with, by its phase:
// step 8's mapping, so a red outcome is never carried by a 2xx and
// `verified` says "keep polling" (DESIGN-box.md, "Handler contract").
var phaseStatus = map[string]int{
	"verified":    202,
	"no_change":   200,
	"applied":     200,
	"refused":     422,
	"failed":      422,
	"rolled_back": 422,
	"unknown":     422,
}

// validID is the id grammar: the first 32 hex characters of the poll
// secret's sha256, lower case, as hex.EncodeToString writes them.
func validID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validDigest is a hex sha256 as hex.EncodeToString writes it.
func validDigest(s string) bool {
	return len(s) == 64 && validID(s[:32]) && validID(s[32:])
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
	f, err := os.OpenFile(path, flags, 0) //nolint:gosec // a fixed path of the box's (an id only after validID), or the operator's CLI argument
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
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, limit)
	}
	return b, nil
}

// readJSON reads one of the exchange tree's files into v. The tree is
// written by root and by the hotserve uid, never through a symlink.
func readJSON(path string, limit int64, v any) error {
	b, err := readFile(path, limit, false)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func readApplied(dir string) (*applied, error) {
	var a applied
	path := filepath.Join(dir, "applied.json")
	if err := readJSON(path, maxApplied, &a); err != nil {
		return nil, err
	}
	if !proof.IsID(a.SHA) {
		return nil, fmt.Errorf("%s: sha is not a 40-hex commit id", path)
	}
	return &a, nil
}

// readMarker reads `stage/<id>.auth`; id must already be valid.
func readMarker(dir, id string) (*marker, error) {
	var m marker
	path := filepath.Join(dir, "stage", id+".auth")
	if err := readJSON(path, maxMarker, &m); err != nil {
		return nil, err
	}
	if !validDigest(m.SHA256) || m.SHA256[:32] != id {
		return nil, fmt.Errorf("%s: sha256 is not the digest of a poll secret for this id", path)
	}
	return &m, nil
}

// readResult reads `out/<id>.json`; id must already be valid.
func readResult(dir, id string) (*result, error) {
	var r result
	path := filepath.Join(dir, "out", id+".json")
	if err := readJSON(path, maxResult, &r); err != nil {
		return nil, err
	}
	if r.ID != id {
		return nil, fmt.Errorf("%s: names another id", path)
	}
	if _, ok := phaseStatus[r.Phase]; !ok {
		return nil, fmt.Errorf("%s: phase %s is not a result's", path, proof.Bound(r.Phase))
	}
	return &r, nil
}
