package box

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"

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
digest of the installed file stay as they were, so an edit made on the
box since the last apply is still reported as one. It is not the fix
for an unsigned commit above the commit the box runs: that is a rebase
and a force-push, with nothing to do on the box. As root, after
hotserve init; it waits while the applier holds its lock.`,
				Args: cobra.ExactArgs(1),
				RunE: func(c *cobra.Command, args []string) error {
					return runBaseline(commandContext(c), os.Geteuid(), newApplier(caddy.Log().Named("box.baseline")), args[0], c.OutOrStdout())
				},
			})
		},
	})
}

// commandContext is the command's context, or a background one when it
// was run without.
func commandContext(c *cobra.Command) context.Context {
	if ctx := c.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// commitArg holds init's and baseline's argument to a commit id as the
// box records one: 40 lowercase hex.
func commitArg(sha string) error {
	if !proof.IsID(sha) {
		return fmt.Errorf("%s is not a commit id: 40 lowercase hex, as git rev-parse HEAD prints one", proof.Bound(sha))
	}
	return nil
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
