package cli

import (
	"context"

	"github.com/urfave/cli/v3"
)

func configCommand() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "manage donkey's settings on this machine",
		Commands: []*cli.Command{
			{
				Name:  "show",
				Usage: "print the current settings",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					s, err := newSession(cmd)
					if err != nil {
						return err
					}
					u := s.UI
					if s.Resolved.Global == nil || s.Resolved.Global.Server.URL == "" {
						u.Printf("Not connected yet — run %s.", u.Bold("donkey login"))
						return nil
					}
					u.Printf("Settings file: %s", s.GlobalPath)
					u.Printf("  Server: %s", s.Resolved.Global.Server.URL)
					if s.Resolved.Global.KubeContext != "" {
						u.Printf("  Kube context (for server install): %s", s.Resolved.Global.KubeContext)
					}
					return nil
				},
			},
		},
	}
}
