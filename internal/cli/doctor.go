package cli

import (
	"context"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/doctor"
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
			env := doctor.NewEnv(s.Resolved.Flags.AppDir)
			u.Printf("%s", u.Bold("Checking your setup…"))
			u.Printf("")

			ok := doctor.Report(ctx, u, env, []*doctor.Check{
				doctor.ConnectionCheck(func(ctx context.Context) error {
					api, err := s.api()
					if err != nil {
						return err
					}
					return api.Ping(ctx)
				}, s.Server.URL),
				doctor.AppConfigCheck(),
			})

			// The server contributes its own view (build engine, registry,
			// assistant). Failing to fetch those checks is itself a finding —
			// an all-clear without them would be a lie.
			if api, err := s.api(); err == nil {
				checks, _, derr := api.Doctor(ctx)
				if derr != nil {
					ok = false
					u.Fail("Server-side checks — couldn't run them: %v", derr)
				}
				for _, c := range checks {
					if c.OK {
						u.Success("%s%s", c.Name, doctor.Suffix(c.Detail))
					} else {
						ok = false
						u.Fail("%s — %s", c.Name, c.Detail)
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
