// Package restore is what runs inside the unit that handles a snapshot
// once it is back on the box: it reads the snapshot's own plan.json,
// checks every database copy, and — told to — installs the copies and
// the files into a directory. It runs as the app's own user, with no
// network and no credential, seeing what was fetched and one app's
// shared dir.
//
// A restore puts back what the snapshot holds: files, directories and
// the symbolic links between them, each with the mode it had, and a
// file with several names as one file with those names. Only what a
// restore could not put back and an app could not have as data — a
// FIFO, a socket, a device — is listed and left out. Not put back:
// setuid, setgid and sticky bits (the unit cannot set them), extended
// attributes and ACLs, and directories' times.
//
// Every check comes before any change. What is restored into is the
// app's own to rearrange while this runs, so nothing in it is reached by
// name: every directory is walked one component at a time without
// following a link (nofollow), and a file or a link lands beside its
// name and is renamed onto it, which replaces what is there and never
// writes through a link. What was fetched is this run's own scratch,
// under a directory no app can enter, so a copy closed to its owner is
// opened to be read (a backup reads with a capability; a restore is the
// owner) and the target still gets the mode the snapshot held. Nothing
// of the app's is removed — not even what looks like a file a killed
// restore left, which an app may well have named so itself — only the
// sidecars of a database that is no longer there.
package restore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/smallhoursorg/hotserve/backups/dump"
	"github.com/smallhoursorg/hotserve/backups/nofollow"
	"github.com/smallhoursorg/hotserve/liveswap/backupdecl"
	"golang.org/x/sys/unix"
)

// Class is how one declared item fared.
type Class string

const (
	// OK: checked, and installed where installing was asked for.
	OK Class = "ok"
	// Missing: the snapshot's plan.json declares it and the snapshot
	// does not hold it.
	Missing Class = "missing"
	// Damaged: a database copy that integrity_check does not call ok.
	Damaged Class = "damaged"
	// NotADatabase: a database copy that is not a SQLite file.
	NotADatabase Class = "not a database"
	// Refused: where it would go, something is in the way that a restore
	// does not write through or over.
	Refused Class = "refused"
	// Failed: checking or installing it went wrong.
	Failed Class = "failed"
	// HeldBack: sound, and not installed because something else was not.
	HeldBack Class = "held back"
)

// Known reports whether c is one of the classes above.
func Known(c Class) bool {
	switch c {
	case OK, Missing, Damaged, NotADatabase, Refused, Failed, HeldBack:
		return true
	}
	return false
}

// Item is one path the snapshot's plan.json declares.
type Item struct {
	Kind   string `json:"kind"` // "sqlite" or "files"
	Path   string `json:"path"`
	Class  Class  `json:"class"`
	Detail string `json:"detail,omitempty"`
}

// Skipped is an entry of the snapshot that is neither a file nor a
// directory, which a restore never installs.
type Skipped struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// Answer is what the unit prints.
type Answer struct {
	// Error is why nothing could be said about any item: the snapshot's
	// plan.json is not there, or is not a declaration.
	Error string             `json:"error,omitempty"`
	Plan  *backupdecl.Config `json:"plan,omitempty"`
	Items []Item             `json:"items,omitempty"`
	// Left is what is in place under a declared files path and not in
	// the snapshot; LeftMore, how many more there are than are named.
	Left     []string  `json:"left,omitempty"`
	LeftMore int       `json:"left_more,omitempty"`
	Skipped  []Skipped `json:"skipped,omitempty"`
	// SkippedMore is how many more there are than are named.
	SkippedMore int `json:"skipped_more,omitempty"`
	// Changed says that installing was begun: an item that then failed
	// leaves the app's data partly restored, not as it was.
	Changed bool `json:"changed,omitempty"`
}

// Mode is what is done once everything is checked.
type Mode int

const (
	// CheckOnly installs nothing: a restore drill.
	CheckOnly Mode = iota
	// AllOrNothing installs only if every item is sound and nothing is in
	// its way: a restore into place.
	AllOrNothing
	// WhatIsSound installs every item that is sound: a restore into a
	// directory of its own, to be looked at.
	WhatIsSound
)

// named is how many paths a list names before it only counts.
const named = 100

// planLimit bounds the plan.json that is read into memory; a real one is
// a few hundred bytes.
const planLimit = 1 << 20

type job struct {
	ctx    context.Context
	staged int // what was fetched: plan.json, sqlite/, files/
	target int // -1 when only checking
	// The same two by name, for sqlite3, which takes paths. They are the
	// unit's own mount points; beneath the target's, the names are the
	// app's to re-aim, and lead nowhere but into its own shared dir: the
	// unit's view holds no other app.
	stagedPath, targetPath string
	decl                   *backupdecl.Config
	excluded               map[string]bool // declared databases and their sidecars, relative to the shared dir
	answer                 *Answer
	made                   []entry // the directories this restore made, for their modes
	// placed is where the first name of a staged file with several
	// names went, by its inode, for the names after it.
	placed map[uint64]string
	// madeAt indexes made by rel, and seen is every directory this
	// restore has found in place or made: neither is walked twice.
	madeAt map[string]int
	seen   map[string]bool
	// skipped is every snapshot entry listed as left out, by its place.
	skipped map[string]bool
}

// noPerm is the mode of a directory made only on the way to something:
// nothing known, so nothing set.
const noPerm = ^uint32(0)

// Run checks the snapshot in the directory staged, and installs it into
// target as mode says. target is not looked at when mode is CheckOnly.
func Run(ctx context.Context, staged, target string, mode Mode) Answer {
	var a Answer
	sfd, err := unix.Open(staged, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		a.Error = "what was fetched: " + err.Error()
		return a
	}
	defer unix.Close(sfd) //nolint:errcheck // a path descriptor
	j := &job{ctx: ctx, staged: sfd, target: -1, stagedPath: staged, targetPath: target, answer: &a, excluded: map[string]bool{}}
	if mode != CheckOnly {
		if j.target, err = unix.Open(target, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0); err != nil {
			a.Error = "where to restore into: " + err.Error()
			return a
		}
		defer unix.Close(j.target) //nolint:errcheck // a path descriptor
	}
	if j.decl, err = readPlan(sfd); err != nil {
		a.Error = "the snapshot's plan.json: " + err.Error()
		return a
	}
	a.Plan = j.decl
	for _, db := range j.decl.SQLite {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			j.excluded[db+suffix] = true
		}
	}

	// Every check, then every change.
	for _, p := range j.decl.SQLite {
		a.Items = append(a.Items, j.checkDatabase(p))
	}
	trees := map[string]*tree{}
	for _, p := range j.decl.Files {
		it, t := j.checkFiles(p)
		if it.Class == OK && t != nil {
			// A file or a link where a declared database's directory has
			// to be: the database step makes the directory, and the files
			// step would then fail — after the database was changed.
			if rel, dbPath := j.inTheWayOfADatabase(t); rel != "" {
				it.Class, it.Detail = Refused, fmt.Sprintf("%q is a file in the snapshot where the database %q needs a directory", rel, dbPath)
			}
		}
		a.Items = append(a.Items, it)
		trees[p] = t
	}
	if mode == CheckOnly {
		return a
	}
	sound := true
	for _, it := range a.Items {
		sound = sound && it.Class == OK
	}
	if !sound && mode == AllOrNothing {
		for i := range a.Items {
			if a.Items[i].Class == OK {
				a.Items[i].Class, a.Items[i].Detail = HeldBack, "sound, and not installed: every item is checked before any is installed"
			}
		}
		return a
	}
	a.Changed = true
	for i := range a.Items {
		it := &a.Items[i]
		if it.Class != OK || ctx.Err() != nil {
			continue
		}
		var err error
		if it.Kind == "sqlite" {
			err = j.installDatabase(it.Path)
		} else {
			err = j.installFiles(it.Path, trees[it.Path])
		}
		if err != nil {
			it.Class, it.Detail = Failed, err.Error()
		}
	}
	// Directories opened for the install — made, or closed to writing in
	// place — get their modes last, whatever became of the items.
	if err := j.closeDirs(); err != nil {
		for i := range a.Items {
			if a.Items[i].Class == OK {
				a.Items[i].Class, a.Items[i].Detail = Failed, "in place, and a directory's mode could not be set back: "+err.Error()
			}
		}
	}
	// To the disk once, when everything is in place, and not once a file:
	// a restore that did not reach here is run again.
	err = j.flush()
	for i := range a.Items {
		switch it := &a.Items[i]; {
		case it.Class != OK:
		case ctx.Err() != nil:
			it.Class, it.Detail = Failed, "interrupted"
		case err != nil:
			it.Class, it.Detail = Failed, "in place, and not known to be on the disk: "+err.Error()
		}
	}
	return a
}

// flush writes the filesystem restored into to the disk.
func (j *job) flush() error {
	fd, err := nofollow.Open(j.target, ".", unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer unix.Close(fd) //nolint:errcheck // read-only
	return unix.Syncfs(fd)
}

// readPlan reads the declaration the snapshot was made from. It came out
// of the repository, and is believed no more than the Caddyfile is: a
// regular file of a sane size, decoded strictly, valid by the rules a
// declaration is held to anywhere.
func readPlan(staged int) (*backupdecl.Config, error) {
	fd, err := nofollow.Open(staged, "plan.json", unix.O_RDONLY|unix.O_NONBLOCK)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("the snapshot holds none, so it does not say what it is a snapshot of")
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "plan.json")
	defer f.Close() //nolint:errcheck // read-only
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > planLimit {
		return nil, fmt.Errorf("it is not a file a declaration would be (%s, %d bytes)", st.Mode().Type(), st.Size())
	}
	dec := json.NewDecoder(io.LimitReader(f, planLimit))
	dec.DisallowUnknownFields()
	decl := new(backupdecl.Config)
	if err := dec.Decode(decl); err != nil {
		return nil, err
	}
	// One declaration and nothing after it: a decoder takes the first
	// value and leaves the rest unread.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("something follows the declaration")
	}
	if err := decl.Validate(); err != nil {
		return nil, err
	}
	return decl, nil
}

// checkDatabase looks at the copy of one database, and at what is where
// it would go.
func (j *job) checkDatabase(p string) Item {
	it := Item{Kind: "sqlite", Path: p, Class: OK}
	rel := path.Join("sqlite", p)
	// The copy is this run's scratch; read it even if its own mode does
	// not let the owner (a backup read the live database with a capability).
	if err := j.chmodStaged(rel, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
		it.Class, it.Detail = Failed, "the copy in the snapshot: "+err.Error()
		return it
	}
	switch res := dump.Inspect(j.stagedPath, rel); res.Class {
	case dump.OK:
	case dump.Missing:
		it.Class, it.Detail = Missing, "the snapshot holds no copy of it"
		return it
	case dump.NotADatabase:
		it.Class, it.Detail = NotADatabase, "the copy in the snapshot: "+res.Detail
		return it
	default:
		it.Class, it.Detail = Failed, "the copy in the snapshot: "+res.Detail
		return it
	}
	sound, said := dump.CheckCopy(j.ctx, path.Join(j.stagedPath, rel))
	if j.ctx.Err() != nil {
		it.Class, it.Detail = Failed, "interrupted"
		return it
	}
	if !sound {
		it.Class, it.Detail = Damaged, "the copy in the snapshot is damaged (integrity_check: "+said+")"
		return it
	}
	if j.target < 0 {
		return it
	}
	if err := j.checkTarget(p, false); err != nil {
		it.Class, it.Detail = Refused, err.Error()
		return it
	}
	// A live file sqlite3 could not open — a first page that is noise —
	// would be found when the restore has begun. An empty file it opens
	// as an empty database.
	if res := dump.Inspect(j.targetPath, p); res.Class == dump.NotADatabase && !strings.HasPrefix(res.Detail, "no SQLite header (0 bytes)") {
		it.Class, it.Detail = Refused, fmt.Sprintf("%q in place is not a database sqlite3 can open (%s); move it aside, and a restore puts the copy in its place", p, res.Detail)
		return it
	}
	// Where there is no database, its sidecars' names are removed before
	// the copy goes in; one that is not a file cannot be.
	if !j.exists(p) {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if kind := j.kindAt(p + suffix); kind != 0 && kind != unix.S_IFREG && kind != unix.S_IFLNK {
				it.Class, it.Detail = Refused, fmt.Sprintf("%q is not there, and %q beside it is %s, which a restore does not remove", p, p+suffix, kindWord(kind))
				return it
			}
		}
	}
	return it
}

// checkTarget looks at where rel would go: no symbolic link on the way
// to it; and at it, nothing, or a file — or for a directory, a
// directory. A link where a file goes is replaced, not followed, so it
// is no refusal — except at a database, which sqlite3 opens by name.
func (j *job) checkTarget(rel string, dir bool) error {
	fd, err := nofollow.Open(j.target, rel, unix.O_PATH)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Whatever exists of the way there is looked at all the same.
		if parent := path.Dir(rel); parent != "." {
			return j.checkTarget(parent, true)
		}
		return nil
	case errors.Is(err, nofollow.ErrLink):
		return fmt.Errorf("a symbolic link is in the way of %q, and a restore does not write through one", rel)
	case errors.Is(err, unix.ENOTDIR):
		return fmt.Errorf("something that is not a directory is in the way of %q", rel)
	case err != nil:
		return err
	}
	defer unix.Close(fd) //nolint:errcheck // a path descriptor
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	switch kind := st.Mode & unix.S_IFMT; {
	case dir && kind != unix.S_IFDIR:
		return fmt.Errorf("%q is a directory in the snapshot and %s in place", rel, kindWord(kind))
	case !dir && kind != unix.S_IFREG:
		return fmt.Errorf("%q is a file in the snapshot and %s in place", rel, kindWord(kind))
	}
	return nil
}

// A tree is what the snapshot holds under one declared files path: the
// directories, parents first, and the files, each relative to the shared
// dir.
type tree struct {
	single bool // the declared path is a file
	dirs   []entry
	files  []entry
	links  []link
}

type entry struct {
	rel  string // relative to the shared dir
	perm uint32
	// ino is the staged file's inode where it has more than one name: a
	// hardlinked pair comes back as one file with two names, not two
	// files — as restic put it in the scratch, and as it was.
	ino uint64
}

// link is a symbolic link the snapshot holds: its place, and the text it
// points at, which is stored and never followed.
type link struct {
	rel, target string
}

func (j *job) checkFiles(p string) (Item, *tree) {
	it := Item{Kind: "files", Path: p, Class: OK}
	src := path.Join("files", p)
	fd, err := nofollow.Open(j.staged, src, unix.O_PATH)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		it.Class, it.Detail = Missing, "the snapshot does not hold it"
		return it, nil
	case errors.Is(err, nofollow.ErrLink):
		// The declared path is itself a symbolic link in the snapshot.
		target, lerr := j.readlink(path.Dir(src), path.Base(src))
		if lerr != nil {
			it.Class, it.Detail = Failed, "in the snapshot: "+lerr.Error()
			return it, nil
		}
		t := &tree{single: true, links: []link{{p, target}}}
		if j.target >= 0 {
			if err := j.checkLinkTarget(p); err != nil {
				it.Class, it.Detail = Refused, err.Error()
			}
		}
		return it, t
	case err != nil:
		it.Class, it.Detail = Failed, "in the snapshot: "+err.Error()
		return it, nil
	}
	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	unix.Close(fd) //nolint:errcheck,gosec // a path descriptor
	if err != nil {
		it.Class, it.Detail = Failed, "in the snapshot: "+err.Error()
		return it, nil
	}
	t := &tree{}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		t.single = true
		t.files = []entry{{rel: p, perm: st.Mode & 0o777}}
	case unix.S_IFDIR:
		t.dirs = []entry{{rel: p, perm: st.Mode & 0o777}}
		if err := j.walk(src, p, t); err != nil {
			it.Class, it.Detail = Failed, "in the snapshot: "+err.Error()
			return it, nil
		}
	default:
		// A backup refuses a declared path that is not a file or a
		// directory, so this is a snapshot made some other way. There is
		// nothing of the item to put back, which is not what "restored"
		// means: it is refused, and listed with what is left out.
		if len(j.answer.Skipped) < named {
			j.answer.Skipped = append(j.answer.Skipped, Skipped{Path: p, Kind: kindWord(st.Mode & unix.S_IFMT)})
		} else {
			j.answer.SkippedMore++
		}
		it.Class, it.Detail = Refused, "in the snapshot it is "+kindWord(st.Mode&unix.S_IFMT)+" — the declared path itself — which a restore does not install"
		return it, nil
	}
	if j.target < 0 {
		return it, t
	}
	for _, d := range t.dirs {
		if err := j.checkTarget(d.rel, true); err != nil {
			it.Class, it.Detail = Refused, err.Error()
			return it, t
		}
	}
	for _, l := range t.links {
		if err := j.checkLinkTarget(l.rel); err != nil {
			it.Class, it.Detail = Refused, err.Error()
			return it, t
		}
	}
	for _, f := range t.files {
		// A link at a file's own name is replaced; anything else that is
		// not a file is in the way.
		if err := j.checkTarget(f.rel, false); err != nil && !j.isLinkAt(f.rel) {
			it.Class, it.Detail = Refused, err.Error()
			return it, t
		}
		// A file with a -wal beside it in place is a live database that
		// this snapshot holds as a file: renamed over, it would be read
		// with a log that is not its own.
		if j.exists(f.rel + "-wal") {
			it.Class, it.Detail = Refused, fmt.Sprintf("%q is a live SQLite database in place (a -wal is beside it) that this snapshot holds only as a file: put back while the app has it open, it would be read with a log that is not its own. Nothing here can stop the app: restore before it is first deployed, or restore --to a directory and move the file into place with the app stopped", f.rel)
			return it, t
		}
	}
	return it, t
}

// inTheWayOfADatabase finds, in a files tree, a file or link at a path
// that is a directory on the way to a declared database.
func (j *job) inTheWayOfADatabase(t *tree) (rel, dbPath string) {
	dirs := map[string]string{}
	for _, db := range j.decl.SQLite {
		for d := path.Dir(db); d != "." && d != "/"; d = path.Dir(d) {
			dirs[d] = db
		}
	}
	for _, f := range t.files {
		if db, clash := dirs[f.rel]; clash {
			return f.rel, db
		}
	}
	for _, l := range t.links {
		if db, clash := dirs[l.rel]; clash {
			return l.rel, db
		}
	}
	return "", ""
}

// kindAt is what rel itself is in the target — not anything on the way
// to it, which is not followed — or 0 when there is nothing there.
func (j *job) kindAt(rel string) uint32 {
	dir, err := nofollow.Open(j.target, path.Dir(rel), unix.O_PATH)
	if err != nil {
		return 0
	}
	defer unix.Close(dir) //nolint:errcheck // a path descriptor
	var st unix.Stat_t
	if unix.Fstatat(dir, path.Base(rel), &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return 0
	}
	return st.Mode & unix.S_IFMT
}

func (j *job) exists(rel string) bool   { return j.kindAt(rel) != 0 }
func (j *job) isLinkAt(rel string) bool { return j.kindAt(rel) == unix.S_IFLNK }

// checkLinkTarget refuses a symbolic link's place only where a directory
// is in the way: a link cannot be renamed over one, and a restore does
// not remove the app's data. Anything else there — a file, a link — is
// replaced.
func (j *job) checkLinkTarget(rel string) error {
	if err := j.checkTarget(path.Dir(rel), true); err != nil {
		return err
	}
	switch kind := j.kindAt(rel); kind {
	case 0, unix.S_IFREG, unix.S_IFLNK:
	default:
		return fmt.Errorf("%q is %s in place and a symbolic link in the snapshot", rel, kindWord(kind))
	}
	if j.exists(rel + "-wal") {
		return fmt.Errorf("%q is a live SQLite database in place (a -wal is beside it) that this snapshot holds as a symbolic link: nothing here can stop the app; restore before it is first deployed, or restore --to a directory", rel)
	}
	return nil
}

// readlink reads the text of the symbolic link name under the staged
// directory dir, following no link on the way to it.
func (j *job) readlink(dir, name string) (string, error) {
	dfd, err := nofollow.Open(j.staged, dir, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return "", err
	}
	defer unix.Close(dfd) //nolint:errcheck // a path descriptor
	return readlinkFd(dfd, name)
}

// walk lists the snapshot's directory at src (relative to what was
// fetched), whose place in the shared dir is rel.
func (j *job) walk(src, rel string, t *tree) error {
	if j.ctx.Err() != nil {
		return j.ctx.Err()
	}
	// What was fetched is this run's scratch, under a directory no app can
	// enter, and a directory in it may be closed to its owner: open it to
	// read what is in it. The target still gets the mode the snapshot held.
	dir, err := j.readableStagedDir(src)
	if err != nil {
		return err
	}
	defer unix.Close(dir) //nolint:errcheck // read-only
	names, err := dirNames(dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		childRel := path.Join(rel, name)
		if j.excluded[childRel] {
			// A declared database is restored from its copy, never from
			// what stood in its place under files/.
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return &os.PathError{Op: "stat", Path: childRel, Err: err}
		}
		switch kind := st.Mode & unix.S_IFMT; kind {
		case unix.S_IFREG:
			if j.target >= 0 { // a drill keeps no list: it has nothing to install
				e := entry{rel: childRel, perm: st.Mode & 0o777}
				if st.Nlink > 1 {
					e.ino = st.Ino
				}
				t.files = append(t.files, e)
			}
		case unix.S_IFDIR:
			if j.target >= 0 {
				t.dirs = append(t.dirs, entry{rel: childRel, perm: st.Mode & 0o777})
			}
			if err := j.walk(path.Join(src, name), childRel, t); err != nil {
				return err
			}
		case unix.S_IFLNK:
			target, err := readlinkFd(dir, name)
			if err != nil {
				return err
			}
			if j.target >= 0 {
				t.links = append(t.links, link{childRel, target})
			}
		default:
			if j.skipped == nil {
				j.skipped = map[string]bool{}
			}
			j.skipped[childRel] = true
			if len(j.answer.Skipped) < named {
				j.answer.Skipped = append(j.answer.Skipped, Skipped{Path: childRel, Kind: kindWord(kind)})
			} else {
				j.answer.SkippedMore++
			}
		}
	}
	return nil
}

// readableStagedDir opens a staged directory for reading, giving its
// owner rwx first where it lacks it: what was fetched is scratch to be
// read now and removed after.
func (j *job) readableStagedDir(rel string) (int, error) {
	// Read and search both: a directory that opens (0400) but cannot be
	// searched fails at its first child, not here.
	if err := j.chmodStaged(rel, 0o700); err != nil {
		return -1, err
	}
	return nofollow.Open(j.staged, rel, unix.O_RDONLY|unix.O_DIRECTORY)
}

// chmodStaged gives the owner what it needs of the staged entry rel —
// by name, through its directory opened without following a link,
// since fchmod refuses a path descriptor [measured: EBADF, so the mode-0
// file was never read]. The staged tree is under a directory no app
// can enter, so between the look and the chmod nothing swaps the name.
func (j *job) chmodStaged(rel string, mode uint32) error {
	dir, err := nofollow.Open(j.staged, path.Dir(rel), unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer unix.Close(dir) //nolint:errcheck // a path descriptor
	var st unix.Stat_t
	if err := unix.Fstatat(dir, path.Base(rel), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "stat", Path: rel, Err: err}
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return &os.PathError{Op: "chmod", Path: rel, Err: unix.ELOOP}
	}
	if st.Mode&mode == mode {
		return nil
	}
	if err := unix.Fchmodat(dir, path.Base(rel), st.Mode&0o777|mode, 0); err != nil {
		return &os.PathError{Op: "chmod", Path: rel, Err: err}
	}
	return nil
}

// dirNames reads the names in an open directory, sorted.
func dirNames(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "")
	defer f.Close() //nolint:errcheck // read-only
	names, err := f.Readdirnames(-1)
	sort.Strings(names)
	return names, err
}

// readlinkFd reads the text of the symbolic link name under an open
// directory.
func readlinkFd(dirfd int, name string) (string, error) {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(dirfd, name, buf)
	if err != nil {
		return "", &os.PathError{Op: "readlink", Path: name, Err: err}
	}
	return string(buf[:n]), nil
}

// readDir lists the names in the directory rel beneath dirfd, sorted,
// following no link on the way.
func readDir(dirfd int, rel string) ([]string, error) {
	fd, err := nofollow.Open(dirfd, rel, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd) //nolint:errcheck // read-only
	return dirNames(fd)
}

// installFiles makes the directories and puts the files in place, then
// lists what is in place and not in the snapshot.
func (j *job) installFiles(p string, t *tree) error {
	if t.single {
		// A declared file's directory may not be there yet: a rebuilt box.
		if err := j.mkdir(path.Dir(p), noPerm); err != nil {
			return err
		}
	}
	for _, d := range t.dirs {
		if err := j.mkdir(d.rel, d.perm); err != nil {
			return err
		}
	}
	for _, f := range t.files {
		if j.ctx.Err() != nil {
			return j.ctx.Err()
		}
		src := path.Join("files", f.rel)
		if t.single {
			src = path.Join("files", p)
		}
		if first, seen := j.placed[f.ino]; f.ino != 0 && seen {
			if err := j.hardlink(first, f.rel); err != nil {
				return fmt.Errorf("%q: %w", f.rel, err)
			}
			continue
		}
		if err := j.copyFile(src, f.rel, f.perm); err != nil {
			return fmt.Errorf("%q: %w", f.rel, err)
		}
		if f.ino != 0 {
			if j.placed == nil {
				j.placed = map[uint64]string{}
			}
			j.placed[f.ino] = f.rel
		}
	}
	for _, l := range t.links {
		if j.ctx.Err() != nil {
			return j.ctx.Err()
		}
		if err := j.symlink(l.rel, l.target); err != nil {
			return fmt.Errorf("%q: %w", l.rel, err)
		}
	}
	if !t.single {
		j.left(path.Join("files", p), p)
	}
	return nil
}

// closeDirs gives every directory this restore opened its mode: one it
// made, the snapshot's; one that was closed to writing in place, the one
// it had. Deepest last-opened first, and once, after every item — an
// item that failed, or a database's directory, is no exception.
func (j *job) closeDirs() error {
	// Deepest first, whatever order they were seen in: a parent closed
	// before its child could not be opened to close the child.
	sort.SliceStable(j.made, func(a, b int) bool {
		return strings.Count(j.made[a].rel, "/") > strings.Count(j.made[b].rel, "/")
	})
	var errs []error
	for i := range j.made {
		d := j.made[i]
		fd, err := nofollow.Open(j.target, d.rel, unix.O_RDONLY|unix.O_DIRECTORY)
		if err == nil {
			err = unix.Fchmod(fd, d.perm)
			unix.Close(fd) //nolint:errcheck,gosec // read-only
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%q: %w", d.rel, err))
		}
	}
	j.made, j.madeAt = nil, nil
	return errors.Join(errs...)
}

// madeAlready reports whether this restore made rel, and gives it the
// mode a later item supplies for it — a directory first made on the way
// to something, with 0700 and nothing better known.
func (j *job) madeAlready(rel string, perm uint32) bool {
	i, ok := j.madeAt[rel]
	if ok && perm != noPerm {
		j.made[i].perm = perm
	}
	return ok
}

// record remembers a directory whose mode is to be set last.
func (j *job) record(rel string, perm uint32) {
	if j.madeAt == nil {
		j.madeAt = map[string]int{}
	}
	if i, ok := j.madeAt[rel]; ok {
		j.made[i].perm = perm
		return
	}
	j.madeAt[rel] = len(j.made)
	j.made = append(j.made, entry{rel: rel, perm: perm})
}

// hardlink gives the file already placed at first another name, rel:
// made beside its name and renamed onto it, like a file, in directories
// opened without following a link.
func (j *job) hardlink(first, rel string) error {
	if err := j.mkdir(path.Dir(rel), noPerm); err != nil {
		return err
	}
	from, err := nofollow.Open(j.target, path.Dir(first), unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer unix.Close(from) //nolint:errcheck // a path descriptor
	dir, err := nofollow.Open(j.target, path.Dir(rel), unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer unix.Close(dir) //nolint:errcheck // a path descriptor
	tmp, err := tmpName()
	if err != nil {
		return err
	}
	if err := unix.Linkat(from, path.Base(first), dir, tmp, 0); err != nil {
		return &os.PathError{Op: "link", Path: rel, Err: err}
	}
	if err := unix.Renameat(dir, tmp, dir, path.Base(rel)); err != nil {
		_ = unix.Unlinkat(dir, tmp, 0) // this restore's own
		return err
	}
	return nil
}

// symlink creates the symbolic link rel -> target in the target,
// beside its name and renamed onto it: it replaces whatever is there
// (except a directory, refused at the checks) and follows nothing.
func (j *job) symlink(rel, target string) error {
	if err := j.mkdir(path.Dir(rel), noPerm); err != nil {
		return err
	}
	dir, err := nofollow.Open(j.target, path.Dir(rel), unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer unix.Close(dir) //nolint:errcheck // a path descriptor
	tmp, err := tmpName()
	if err != nil {
		return err
	}
	if err := unix.Symlinkat(target, dir, tmp); err != nil {
		return err
	}
	if err := unix.Renameat(dir, tmp, dir, path.Base(rel)); err != nil {
		_ = unix.Unlinkat(dir, tmp, 0) // this restore's own
		return err
	}
	return nil
}

// mkdir makes the directory rel in the target, and whatever of the way
// there is missing; what exists is left as it is.
func (j *job) mkdir(rel string, perm uint32) error {
	if rel == "." {
		return nil
	}
	if j.seen[rel] && perm == noPerm {
		return nil // looked at already, and nothing new to say of it
	}
	if parent := path.Dir(rel); parent != "." {
		if err := j.mkdir(parent, noPerm); err != nil {
			return err
		}
	}
	dir, err := nofollow.Open(j.target, path.Dir(rel), unix.O_PATH)
	if err != nil {
		return err
	}
	defer unix.Close(dir) //nolint:errcheck // a path descriptor
	if j.seen == nil {
		j.seen = map[string]bool{}
	}
	j.seen[rel] = true
	switch err := unix.Mkdirat(dir, path.Base(rel), 0o700); {
	case err == nil:
		final := perm
		if final == noPerm {
			final = 0o700
		}
		j.record(rel, final) // the unit's umask cuts a mode given here
	case !errors.Is(err, unix.EEXIST):
		return &os.PathError{Op: "mkdir", Path: rel, Err: err}
	case j.madeAlready(rel, perm):
		// Made by this restore for an earlier item, on the way to a
		// database or a file: the mode this item knows is the one it gets.
	default:
		// In place already. It gets the snapshot's mode, last, where the
		// snapshot says one; and one its owner has closed to writing —
		// the restore is its owner — is opened while it is filled, and
		// closed again after, to the snapshot's mode or its own. One
		// carrying a bit above 0777 — setgid on a shared uploads dir — is
		// left with its mode: the unit could not set such a bit back.
		if fd, err := nofollow.Open(j.target, rel, unix.O_RDONLY|unix.O_DIRECTORY); err == nil {
			var st unix.Stat_t
			if unix.Fstat(fd, &st) == nil {
				final := st.Mode & 0o777
				if perm != noPerm && st.Mode&0o7000 == 0 {
					final = perm
				}
				opened := st.Mode&0o300 == 0o300 || unix.Fchmod(fd, st.Mode&0o7777|0o700) == nil
				if opened && (final != st.Mode&0o777 || st.Mode&0o300 != 0o300) {
					j.record(rel, final)
				}
			}
			unix.Close(fd) //nolint:errcheck,gosec // read-only
		}
	}
	// Whatever is there now is a directory, reached through no link.
	fd, err := nofollow.Open(j.target, rel, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// copyFile puts the fetched file src at rel in the target: written
// beside its name, in the directory opened without following a link, and
// renamed onto it — which replaces a link at that name, and never writes
// through one.
func (j *job) copyFile(src, rel string, perm uint32) error {
	in, err := nofollow.Open(j.staged, src, unix.O_RDONLY|unix.O_NONBLOCK)
	if errors.Is(err, unix.EACCES) {
		// Scratch, this run's own; the target gets perm, not this.
		if err := j.chmodStaged(src, 0o600); err != nil {
			return err
		}
		in, err = nofollow.Open(j.staged, src, unix.O_RDONLY|unix.O_NONBLOCK)
	}
	if err != nil {
		return err
	}
	from := os.NewFile(uintptr(in), src)
	defer from.Close() //nolint:errcheck // read-only
	st, err := from.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("it is no longer a regular file in what was fetched")
	}
	dir, err := nofollow.Open(j.target, path.Dir(rel), unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer unix.Close(dir) //nolint:errcheck // a path descriptor
	tmp, err := tmpName()
	if err != nil {
		return err
	}
	out, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	to := os.NewFile(uintptr(out), tmp)
	err = func() error {
		if _, err := io.Copy(to, from); err != nil {
			return err
		}
		if err := to.Chmod(fs.FileMode(perm)); err != nil {
			return err
		}
		mtime := unix.NsecToTimespec(st.ModTime().UnixNano())
		return unix.UtimesNanoAt(dir, tmp, []unix.Timespec{mtime, mtime}, unix.AT_SYMLINK_NOFOLLOW)
	}()
	if cerr := to.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = unix.Renameat(dir, tmp, dir, path.Base(rel))
	}
	if err != nil {
		_ = unix.Unlinkat(dir, tmp, 0) // this restore's own, and nothing else's
		return err
	}
	return nil
}

// tmpPrefix marks a file a restore is writing. One that a killed restore
// left is the app's from then on, like anything else under its name: it
// is listed as left in place, backed up, and restored — a name is never
// a reason to remove a file or to leave one out, since an app may call
// a file of its own anything.
const tmpPrefix = ".hotserve-restore-"

func tmpName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return tmpPrefix + hex.EncodeToString(b), nil
}

// left lists what is in place under rel and not in the snapshot's src. It
// goes no deeper than a directory the snapshot does not hold: that
// directory is what is named. An error ends the listing, not the
// restore: everything is already in place.
func (j *job) left(src, rel string) {
	inPlace, err := readDir(j.target, rel)
	if err != nil {
		return
	}
	held := map[string]bool{}
	if dir, err := j.readableStagedDir(src); err == nil {
		names, _ := dirNames(dir)
		unix.Close(dir) //nolint:errcheck,gosec // read-only
		for _, n := range names {
			held[n] = true
		}
	}
	for _, name := range inPlace {
		childRel := path.Join(rel, name)
		if j.excluded[childRel] {
			continue
		}
		if !held[name] || j.skipped[childRel] {
			// Not in the snapshot — or in it as something a restore does
			// not install, so neither overwritten nor to be left unsaid.
			if len(j.answer.Left) < named {
				j.answer.Left = append(j.answer.Left, childRel)
			} else {
				j.answer.LeftMore++
			}
			continue
		}
		if fd, err := nofollow.Open(j.target, childRel, unix.O_PATH|unix.O_DIRECTORY); err == nil {
			unix.Close(fd) //nolint:errcheck,gosec // a path descriptor
			j.left(path.Join(src, name), childRel)
		}
	}
}

// installDatabase restores the checked copy of p over the live database
// (dump.RestoreOver), or — where there is none — puts the copy there.
func (j *job) installDatabase(p string) error {
	if err := j.mkdir(path.Dir(p), noPerm); err != nil {
		return err
	}
	copyOf := path.Join("sqlite", p)
	live, err := nofollow.Open(j.target, p, unix.O_PATH)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Sidecars of a database that is gone would be read as this one's.
		dir, err := nofollow.Open(j.target, path.Dir(p), unix.O_PATH|unix.O_DIRECTORY)
		if err != nil {
			return err
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if err := unix.Unlinkat(dir, path.Base(p)+suffix, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				unix.Close(dir) //nolint:errcheck,gosec // a path descriptor
				return &os.PathError{Op: "remove", Path: p + suffix, Err: err}
			}
		}
		unix.Close(dir) //nolint:errcheck,gosec // a path descriptor
		// Nothing here can stop the app: one that re-creates its database
		// meanwhile keeps writing to a file that is no longer there.
		return j.copyFile(copyOf, p, 0o600)
	case errors.Is(err, nofollow.ErrLink):
		return fmt.Errorf("a symbolic link is in the way of %q, and a restore does not write through one", p)
	case err != nil:
		return err
	}
	var st unix.Stat_t
	err = unix.Fstat(live, &st)
	unix.Close(live) //nolint:errcheck,gosec // a path descriptor
	if err != nil {
		return err
	}
	// What it was when it was checked, it may not be now. A file sqlite3
	// may open — an empty one is an empty database — and says so if it
	// cannot; anything else it is never started on.
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%q is %s in place, not a database", p, kindWord(st.Mode&unix.S_IFMT))
	}
	mark, err := tmpName()
	if err != nil {
		return err
	}
	marker := path.Join(os.TempDir(), mark) // the unit's own /tmp
	defer os.Remove(marker)                 //nolint:errcheck // gone with the unit in any case
	err = dump.RestoreOver(j.ctx, path.Join(j.targetPath, p), path.Join(j.stagedPath, copyOf), marker)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, dump.ErrNeverOpened):
		return fmt.Errorf("sqlite3 had not opened the live database after %s and was killed: the path stopped being a file it could open", dump.OpenWithin())
	case j.ctx.Err() != nil:
		return errors.New("interrupted")
	case errors.Is(err, dump.ErrBusy):
		return errors.New("the live database stayed locked: in rollback-journal mode a restore waits for the app's writes to pause; stop the app, or try again")
	}
	return fmt.Errorf("sqlite3 could not restore over the live database; move it aside, and a restore puts the copy in its place: %w", err)
}

func kindWord(kind uint32) string {
	switch kind {
	case unix.S_IFDIR:
		return "a directory"
	case unix.S_IFREG:
		return "a file"
	case unix.S_IFLNK:
		return "a symbolic link"
	case unix.S_IFIFO:
		return "a fifo"
	case unix.S_IFSOCK:
		return "a socket"
	case unix.S_IFCHR, unix.S_IFBLK:
		return "a device"
	}
	return "a special file"
}
