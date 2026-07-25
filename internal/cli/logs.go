package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/config"
)

func logsCommand() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "show what the app is writing (add an addon name for its logs)",
		ArgsUsage: "[addon-name]",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "keep showing new lines as they appear"},
			&cli.IntFlag{Name: "tail", Value: 200, Usage: "how many recent lines to show"},
			&cli.DurationFlag{Name: "since", Usage: "only lines newer than this (e.g. 1h, 30m)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			app, err := requireApp(s)
			if err != nil {
				return err
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			component := "web"
			if name := cmd.Args().First(); name != "" {
				var entry *config.AddonConfig
				for i, a := range app.Addons {
					if a.Name == name {
						entry = &app.Addons[i]
						break
					}
				}
				if entry == nil {
					return fmt.Errorf("%s isn't an addon of %s — `donkey addon list` shows what's attached", name, app.Name)
				}
				if entry.External {
					return fmt.Errorf("%s runs elsewhere (attached, not managed by donkey) — its logs live wherever it is hosted", name)
				}
				component = "addon-" + name
			}
			return api.Logs(ctx, s.Space, app.Name, component, cmd.Bool("follow"), int(cmd.Int("tail")), cmd.Duration("since"), os.Stdout)
		},
	}
}
