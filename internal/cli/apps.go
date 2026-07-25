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
				// Three states, three glyphs: healthy (green), unhealthy
				// (red), and "the control plane couldn't check just now" —
				// which must not look like an outage.
				glyph, note := u.Green("●"), ""
				switch {
				case app.Ready == "status unavailable":
					glyph, note = u.Faint("◌"), u.Faint(" (status unknown)")
				case !app.Healthy:
					glyph, note = u.Red("○"), u.Faint(" ("+app.Ready+")")
				}
				u.Printf("  %s %-24s %s%s", glyph, app.Name, u.Faint(app.URL), note)
			}
			u.Printf("")
			return nil
		},
	}
}
