package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// Passthrough runs restic with the repository settings this box
// already has, as the user that owns the backups.
//
// It exists because the alternative is worse in both directions: the
// settings live in a root-only file, so `restic snapshots` as the
// hotserve user finds no repository at all, and running restic as
// root against a repository on this box leaves root-owned files and
// locks in it that the hourly jobs then cannot remove. Every restore
// in docs/backups.md goes through here, which is also what lets the
// e2e suite run those commands exactly as written.
func Passthrough(ctx context.Context, envFile, username string, args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return fmt.Errorf("say what to run, e.g. `hotserve backup restic -- snapshots --tag hotserve`")
	}
	env, err := LoadEnvFile(envFile)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "restic", args...) //nolint:gosec // a fixed program; the arguments are what the operator typed after `--` on their own command line
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if os.Geteuid() == 0 && username != "" {
		cred, home, err := credentialFor(username)
		if err != nil {
			return err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		// And that user's HOME, as `sudo -H` would: root's is where
		// sudo leaves it, and restic keeps its cache under HOME — so
		// every command printed "unable to open cache: mkdir
		// /root/.cache: permission denied" and ran without one.
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	say(stderr, "+ restic %s", quoteArgs(args))
	return cmd.Run()
}

func credentialFor(username string) (*syscall.Credential, string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, "", fmt.Errorf("looking up the %s user (backups belong to it): %w", username, err)
	}
	// Parsed at the width the kernel uses, rather than parsed as an int
	// and narrowed: a uid that does not fit is a broken passwd entry,
	// and narrowing it would silently run restic as somebody else.
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, "", fmt.Errorf("user %s has a uid %q that is not a number in range", username, u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, "", fmt.Errorf("user %s has a gid %q that is not a number in range", username, u.Gid)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, u.HomeDir, nil
}
