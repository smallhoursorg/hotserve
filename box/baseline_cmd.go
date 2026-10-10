package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
)

// runBaseline is `hotserve box baseline <sha>` (DESIGN-box.md, "`box
// baseline`"): a trust reset, for a history rewritten below the commit
// the box runs or a chain past the cap. Under root's lock, after
// recovery has settled any record, one atomic write of applied.json and
// no record (I5): sha from the argument; path and sha256 as found, so a
// hand edit made since the last apply is still out of band at the next
// push; signer "baseline"; when now. Before init there is nothing to
// reset.
func runBaseline(ctx context.Context, euid int, a *Applier, sha string, out io.Writer) error {
	if euid != 0 {
		return errors.New("hotserve box baseline runs as root: it rewrites the box's baseline")
	}
	if err := commitArg(sha); err != nil {
		return err
	}
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.recoverAll(ctx); err != nil {
		return err
	}
	old, err := readApplied(a.x(""))
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New(msgNoBaseline)
	}
	if err != nil {
		return err
	}
	if old.SHA == sha {
		// Rewriting it would only lose who verified it, and when.
		_, err = fmt.Fprintf(out, "baseline: this box already runs %s; nothing changed\n", shortSHA(sha))
		return err
	}
	if err := a.putApplied(applied{SHA: sha, Path: old.Path, SHA256: old.SHA256, Signer: "baseline", When: a.clock.Now().UTC()}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "baseline: this box ran %s; it now applies commits descending from %s\n", shortSHA(old.SHA), shortSHA(sha))
	return err
}
