package cli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"monobox.ch/donkey/internal/config"
)

func statusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show how this folder's app is doing",
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
			st, err := api.Status(ctx, s.Space, app.Name)
			if err != nil {
				return fmt.Errorf("couldn't ask the server about %s: %w", app.Name, err)
			}
			u := s.UI
			if !st.Found {
				u.Info("%s hasn't been deployed yet — run %s.", u.Bold(app.Name), u.Bold("donkey deploy"))
				return nil
			}
			if st.Healthy {
				u.Success("%s is healthy (%s)", u.Bold(app.Name), st.Ready)
			} else {
				u.Warn("%s is not fully up (%s)", u.Bold(app.Name), st.Ready)
			}
			if st.URL != "" {
				u.Printf("  Address: %s", st.URL)
			}
			u.Printf("  Version: %s", st.Image)
			for _, p := range st.Pods {
				u.Printf("  %s", u.Faint(p))
			}
			if !st.Healthy {
				u.Printf("")
				u.Printf("Run %s to see what the app is writing, or %s for a full check.", u.Bold("donkey logs"), u.Bold("donkey doctor"))
			}
			return nil
		},
	}
}

// requireApp returns the folder's app config or a friendly error.
func requireApp(s *session) (*config.AppConfig, error) {
	if s.Resolved.App == nil {
		return nil, fmt.Errorf("this folder has no donkey.yaml — move into your project folder, or run `donkey deploy` to set one up")
	}
	return s.Resolved.App, nil
}
