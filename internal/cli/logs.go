package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/urfave/cli/v3"
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
				found := false
				for _, a := range app.Addons {
					if a.Name == name {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("%s isn't an addon of %s — `donkey addon list` shows what's attached", name, app.Name)
				}
				component = "addon-" + name
			}
			return api.Logs(ctx, s.Space, app.Name, component, cmd.Bool("follow"), int(cmd.Int("tail")), time.Duration(cmd.Duration("since")), os.Stdout)
		},
	}
}
