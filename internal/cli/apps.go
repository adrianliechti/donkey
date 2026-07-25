package cli

import (
	"context"

	"github.com/urfave/cli/v3"
)

func appsCommand() *cli.Command {
	return &cli.Command{
		Name:  "apps",
		Usage: "list everything deployed, by space",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			apps, err := api.Apps(ctx)
			if err != nil {
				return err
			}
			u := s.UI
			if len(apps) == 0 {
				u.Info("Nothing deployed yet — run %s inside a project folder.", u.Bold("donkey deploy"))
				return nil
			}
			// The server sorts by space, then name, so a change of space is
			// simply the next heading.
			space := ""
			for _, app := range apps {
				if app.Space != space {
					space = app.Space
					u.Printf("")
					u.Printf("%s", u.Bold(space))
				}
				glyph := u.Faint("○")
				if app.Healthy {
					glyph = "●"
				}
				u.Printf("  %s %-24s %s", glyph, app.Name, u.Faint(app.URL))
			}
			u.Printf("")
			return nil
		},
	}
}
