package backup

import (
	"context"
	"fmt"
	"os"
)

// PassthroughArgs is the systemd-run invocation behind `hotserve backup
// restic -- …`: restic with the repository settings this box already
// has, as the user the backups belong to.
//
// The settings are in a root-only file, so `restic snapshots` as the
// hotserve user finds no repository at all; and run as that user,
// restic's cache and anything else it writes belong to the user the
// hourly jobs run as. systemd does both — reads the file for the unit,
// as it does for a job, and runs it as User=, with that user's HOME —
// so nothing here parses the one or switches to the other. Every
// restore in docs/backups.md goes through here, which is also what lets
// the e2e suite run those commands exactly as written.
//
// Not the jobs' sandbox: this is an operator's tool, and what they ask
// of it — a restore into a directory of their choosing — needs the
// filesystem. --pty with --pipe is a terminal when there is one (restic
// asks for a new key's password) and the caller's streams when there is
// not (a script reading `snapshots --json`).
func PassthroughArgs(envFile, username string, args []string) []string {
	argv := []string{
		"--wait", "--collect", "--quiet", "--pty", "--pipe", "--expand-environment=no",
		"--property=User=" + username,
		"--property=EnvironmentFile=" + envFile,
		// The settings are in this process's environment, and it runs as
		// the uid the internet-facing process has. A user namespace of
		// its own is what keeps that uid out of its /proc/<pid>/environ
		// (measured, systemd 257: readable without, denied with). The
		// filesystem stays the operator's to restore into.
		"--property=PrivateUsers=yes",
		// Looked up on the unit's PATH, as the jobs' restic is.
		"/bin/sh", "-c", probeScript, "restic",
	}
	return append(argv, args...)
}

// Passthrough runs it, with the caller's terminal or streams.
func Passthrough(ctx context.Context, envFile, username string, args []string, x Exec) error {
	if len(args) == 0 {
		return fmt.Errorf("say what to run, e.g. `hotserve backup restic -- snapshots --tag hotserve`")
	}
	if err := requireSettingsFile(envFile); err != nil {
		return err
	}
	say(os.Stderr, "+ restic %s", quoteArgs(args))
	return x(ctx, Cmd{Name: "systemd-run", Args: PassthroughArgs(envFile, username, args), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
}
