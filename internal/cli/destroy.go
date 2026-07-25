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
			&cli.BoolFlag{Name: "keep-data", Usage: "keep stored data (databases) and the namespace"},
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
			warning := fmt.Sprintf("Remove %s from the cluster, INCLUDING all stored data?", app.Name)
			if keepData {
				warning = fmt.Sprintf("Remove %s from the cluster (stored data is kept)?", app.Name)
			}
			if !u.AssumeYes && !u.Confirm(warning, false) {
				u.Info("Nothing changed.")
				return nil
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			if err := api.Destroy(ctx, app.Name, keepData); err != nil {
				return fmt.Errorf("removing %s didn't fully work: %w", app.Name, err)
			}
			u.Success("%s has been removed from the cluster.", app.Name)
			u.Info("The donkey.yaml in this folder is untouched — `donkey deploy` brings the app back.")
			return nil
		},
	}
}
