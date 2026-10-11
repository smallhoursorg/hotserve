package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// `hotserve box …` is one command with subcommands: webhook, apply and
// baseline.
func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "box",
		Usage: "<command>",
		Short: "The box config channel: config by signed push",
		Long: `Commands of the box config channel (box/DESIGN-box.md in the
hotserve repository): the box's Caddyfile is written in the operator's
repository, and a signed push reaches the box through box_webhook.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.AddCommand(&cobra.Command{
				Use:   "webhook <Caddyfile>",
				Short: "Print the URL of the Caddyfile's box_webhook",
				Long: `Reads the Caddyfile the way the box does — its raw tokens, expanding
nothing and following nothing — and prints https://<host>/ for the one
site carrying box_webhook. It refuses the file's contents for every
reason the box would, so the laptop, CI and the box cannot disagree about which
address a file names. The file may be a pipe:

    hotserve box webhook <(git show HEAD:deploy.example.com/Caddyfile)`,
				Args: cobra.ExactArgs(1),
				RunE: func(c *cobra.Command, args []string) error {
					url, err := webhookURL(args[0])
					if err != nil {
						return err
					}
					_, err = fmt.Fprintln(c.OutOrStdout(), url)
					return err
				},
			})
			cmd.AddCommand(&cobra.Command{
				Use:   "apply",
				Short: "Apply the config pushes waiting on this box (root; hotserve-box-apply.service runs it)",
				Long: `Settles anything a crash left, takes every bundle box_webhook dropped
into /var/lib/hotserve-box/in, proves each against the Caddyfile this box
runs and installs it with systemctl reload hotserve, rolling back on a
failed reload; then writes each push's result for the workflow's poll
and sweeps old results. One shot, as root: hotserve-box-apply.path starts
it. It exits non-zero only when the previous Caddyfile could not be put
back after a failed install (a full disk); the journal says so.`,
				Args: cobra.NoArgs,
				RunE: func(c *cobra.Command, _ []string) error {
					return runApply(c.Context(), os.Geteuid(), newApplier(caddy.Log().Named("box.apply")))
				},
			})
			cmd.AddCommand(&cobra.Command{
				Use:   "baseline <sha>",
				Short: "Reset the commit this box's pushes must descend from (root)",
				Long: `A trust reset, for the one case where the repository's history no
longer contains the commit this box runs: a rewrite below it, or a
chain longer than 500 commits. Every later push must descend from
<sha>, and the box never examines <sha>'s ancestors. The path and the
digest recorded at the last apply stay as they were, so an edit made
on the box since then is still reported as one. It is not the fix
for an unsigned commit above the commit the box runs: that is a rebase
and a force-push, with nothing to do on the box. As root, after
hotserve init; it waits while the applier holds its lock.`,
				Args: cobra.ExactArgs(1),
				RunE: func(c *cobra.Command, args []string) error {
					return runBaseline(c.Context(), os.Geteuid(), newApplier(consoleLog("box.baseline")), args[0], c.OutOrStdout())
				},
			})
		},
	})
}

// `hotserve init` stands alone: the console's one step in a box's life
// on the normal path, beside run and reload rather than under box.
func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "init",
		Usage: "<sha>",
		Short: "Install this directory's Caddyfile as the box's, and its first baseline (root)",
		Long: `Run as root inside the box's directory, copied over from the repository
(deploy.example.com/ for the box at deploy.example.com; init refuses a
directory with any other name), with the commit you are about to push. From the repository on your laptop, where the
$(...) is expanded (the box has no git), copying the commit's files
rather than the working tree's:

    git archive HEAD deploy.example.com | ssh root@box.example.com tar -xf -
    ssh root@box.example.com "cd deploy.example.com && hotserve init $(git rev-parse HEAD)"

It reads ./Caddyfile the way the box reads every push, records its
path in the repository (<host>/Caddyfile, from its box_webhook host),
installs it as /etc/hotserve/Caddyfile and reloads hotserve. The reload
is the check: a file hotserve will not load is rolled back, the old
configuration serving throughout, and init exits non-zero. hotserve
must be running (on a new box: systemctl enable --now hotserve); if it
is not, init refuses and says how to start it and run init again. From then on the box applies only signed commits that descend
from <sha>. Running it again re-seeds the box: a new host (its
directory renamed with it), or the way back from a Caddyfile that
lists no signer.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.Args = cobra.ExactArgs(1)
			cmd.RunE = func(c *cobra.Command, args []string) error {
				return runInitCmd(c.Context(), os.Geteuid(), newApplier(consoleLog("box.init")), ".", args[0], c.OutOrStdout())
			}
		},
	})
}

// commitArg holds init's and baseline's argument to a commit id as the
// box records one: 40 lowercase hex.
func commitArg(sha string) error {
	if !proof.IsID(sha) {
		return fmt.Errorf("%s is not a commit id: 40 lowercase hex, as git rev-parse HEAD prints one", proof.Bound(sha))
	}
	return nil
}

// consoleLog is the logger of a console command, which writes to the
// terminal it runs in: warnings and errors (recovery's, a rollback's),
// not the per-push info line the applier's journal carries — the
// command prints its outcome itself.
func consoleLog(name string) *zap.Logger {
	return caddy.Log().Named(name).WithOptions(zap.IncreaseLevel(zapcore.WarnLevel))
}

// printf writes one of the console's lines.
func printf(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}

// shortSHA is a commit id as the console's lines print it.
func shortSHA(sha string) string { return sha[:min(len(sha), 7)] }

// runApply is `hotserve box apply`: as root, one run.
func runApply(ctx context.Context, euid int, a *Applier) error {
	if euid != 0 {
		return errors.New("hotserve box apply runs as root: it is hotserve-box-apply.service's command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return a.Run(ctx)
}

// webhookURL is `hotserve box webhook`: the walk on the file at path,
// read whole at most the Caddyfile cap. The path is the operator's own
// argument, so unlike the box's state files it may be a pipe — `<(git
// show HEAD:deploy.example.com/Caddyfile)`, /dev/stdin — and is read
// until its end.
func webhookURL(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the operator's own CLI argument
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	file, err := readCapped(f, path, proof.MaxCaddyfile)
	if err != nil {
		return "", err
	}
	shape, err := Walk(file)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return "https://" + shape.Host + "/", nil
}
