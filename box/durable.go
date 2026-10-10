package box

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// hooks are the test seams at every durable write the applier makes
// (DESIGN-box.md, "Failure-mode table"): fail runs before the write and,
// returning an error, makes the write fail with it; crash runs once the
// write is durable, and a test panics there to stand for a kill or a
// power loss. One point fails after its write has landed: take:sync,
// the take's directory fsyncs, which come after the rename. Nothing the
// applier defers writes to disk, so a panic unwinding the stack changes
// nothing a real crash would not have.
// Production leaves both nil.
type hooks struct {
	fail  func(point string) error
	crash func(point string)
	// read runs once a bundle has been read into memory, before anything
	// is checked: a test writes to the file there, as a writer holding a
	// descriptor from before the rename could.
	read func(id string)
}

func (a *Applier) fail(point string) error {
	if a.hooks.fail != nil {
		return a.hooks.fail(point)
	}
	return nil
}

func (a *Applier) crash(point string) {
	if a.hooks.crash != nil {
		a.hooks.crash(point)
	}
}

// writeDurable writes data to path whole, as I8 says: a temporary in the
// same directory, written, given its mode (after the write, so the umask
// has no say), fsynced and closed, renamed over path, and the directory
// fsynced. Every directory it writes in is root's alone, so the
// temporary's name is fixed and an existing one is truncated. On an
// error before the rename the temporary is removed; after it — the
// directory's fsync — the new bytes are visible but not known durable,
// and the caller reads the file back where that matters (the swap).
func (a *Applier) writeDurable(point, path, tmp string, data []byte, mode os.FileMode) error {
	if err := a.fail(point); err != nil {
		return err
	}
	if err := writeAtomic(path, tmp, data, mode); err != nil {
		return err
	}
	a.crash(point)
	return nil
}

func writeAtomic(path, tmp string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode) //nolint:gosec // a fixed name in a directory only root writes
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp) // best effort: a leftover is swept at the next recovery
		return err
	}
	return syncDir(filepath.Dir(path))
}

// removeDurable removes path and fsyncs its directory. An unlink cannot
// fail for space, so a failure is retried once (the Failure-mode
// table's "retry"); a missing file is already removed. A directory or
// anything else a hostile creator left is removed whole: the applier's
// CAP_DAC_OVERRIDE and CAP_FOWNER are what let it.
func (a *Applier) removeDurable(point, path string) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = a.fail(point); err == nil {
			err = os.RemoveAll(path)
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	a.crash(point)
	return nil
}

// syncDir fsyncs a directory, so a rename or an unlink in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // one of the box's fixed directories
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// exists reports whether something stands at path, without following a
// symlink there: an entry in in/ or work/ of any kind blocks the
// stranded-marker rule (Retention).
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
