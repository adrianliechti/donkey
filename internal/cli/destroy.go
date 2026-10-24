package cli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

func destroyCommand() *cli.Command {
	return &cli.Command{
		Name:  "destroy",
		Usage: "remove this app and everything donkey created for it from the cluster",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "keep-data", Usage: "keep the app's disk, credentials, and build and source history for a redeploy (the space and its services are never touched)"},
			&cli.BoolFlag{Name: "force", Usage: "don't ask for confirmation (for scripts)"},
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
			u := s.UI
			keepData := cmd.Bool("keep-data")
			warning := fmt.Sprintf("Remove %s from the cluster, INCLUDING all stored data and its build and source history?", app.Name)
			if keepData {
				warning = fmt.Sprintf("Remove %s from the cluster (stored data and history are kept)?", app.Name)
			}
			// Deleting data needs an explicit yes (or --force) — the blanket
			// --yes flag only ever answers a prompt with its default, and the
			// default here is "no".
			if !cmd.Bool("force") && !u.Confirm(warning, false) {
				if !u.Interactive || u.AssumeYes {
					// No prompt was possible — a script must hear "this did
					// not happen" as an error, not as success.
					return fmt.Errorf("removing %s needs confirmation — pass --force to skip the prompt", app.Name)
				}
				u.Info("Nothing changed.")
				return nil
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			if err := api.Destroy(ctx, s.Space, app.Name, keepData); err != nil {
				return fmt.Errorf("removing %s didn't fully work: %w", app.Name, err)
			}
			u.Success("%s has been removed from the cluster.", app.Name)
			u.Info("The donkey.yaml in this folder is untouched — `donkey deploy` brings the app back.")
			return nil
		},
	}
}
