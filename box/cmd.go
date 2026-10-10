package box

import (
	"fmt"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"

	"github.com/smallhoursorg/hotserve/box/proof"
)

// `hotserve box …` is one command with subcommands, so the ones later
// PRs add (apply, baseline, edit) sit beside webhook.
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
site carrying box_webhook. It refuses the file for every reason the box
would, so the laptop, CI and the box cannot disagree about which
address a file names.`,
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
		},
	})
}

// webhookURL is `hotserve box webhook`: the walk on the file at path,
// read whole at most the Caddyfile cap.
func webhookURL(path string) (string, error) {
	file, err := readFile(path, proof.MaxCaddyfile, true)
	if err != nil {
		return "", err
	}
	shape, err := Walk(file)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return "https://" + shape.Host + "/", nil
}
