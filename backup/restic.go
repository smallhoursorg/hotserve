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
	cmd := exec.CommandContext(ctx, "restic", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if os.Geteuid() == 0 && username != "" {
		cred, err := credentialFor(username)
		if err != nil {
			return err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	fmt.Fprintf(stderr, "+ restic %s\n", quoteArgs(args))
	return cmd.Run()
}

func credentialFor(username string) (*syscall.Credential, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("looking up the %s user (backups belong to it): %w", username, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, fmt.Errorf("user %s has a non-numeric uid %q", username, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, fmt.Errorf("user %s has a non-numeric gid %q", username, u.Gid)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}
