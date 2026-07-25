package cli

import (
	"context"

	"github.com/urfave/cli/v3"

	"monobox.ch/donkey/internal/config"
	"monobox.ch/donkey/internal/doctor"
)

func doctorCommand() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "check that everything donkey needs is in place, and explain how to fix what isn't",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			u := s.UI
			env := doctor.NewEnv(s.Resolved.Global, s.GlobalPath, s.Resolved.Flags.AppDir, s.Resolved.App)
			u.Printf("%s", u.Bold("Checking your setup…"))
			u.Printf("")

			ok := doctor.Report(ctx, u, env, []*doctor.Check{
				doctor.ConnectionCheck(func(ctx context.Context) error {
					api, err := s.api()
					if err != nil {
						return err
					}
					return api.Ping(ctx)
				}, serverURL(s.Resolved.Global)),
				doctor.AppConfigCheck(),
			})

			// The server contributes its own view (build engine, registry, AI).
			if api, err := s.api(); err == nil {
				if checks, _, derr := api.Doctor(ctx); derr == nil {
					for _, c := range checks {
						if c.OK {
							u.Success("%s%s", c.Name, suffixDetail(c.Detail))
						} else {
							ok = false
							u.Fail("%s — %s", c.Name, c.Detail)
						}
					}
				}
			}

			if !ok {
				return cli.Exit("", 1)
			}
			u.Printf("")
			u.Printf("Everything looks good.")
			return nil
		},
	}
}

func serverURL(g *config.GlobalConfig) string {
	if g == nil {
		return ""
	}
	return g.Server.URL
}

func suffixDetail(d string) string {
	if d == "" {
		return ""
	}
	return " — " + d
}
