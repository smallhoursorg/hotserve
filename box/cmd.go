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

// `hotserve box …` is one command with subcommands, so the ones later
// PRs add (baseline, edit) sit beside webhook and apply.
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

    hotserve box webhook <(git show HEAD:box1/Caddyfile)`,
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
		},
	})
}

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
// show HEAD:box1/Caddyfile)`, /dev/stdin — and is read until its end.
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
