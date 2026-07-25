package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"
)

func envCommand() *cli.Command {
	return &cli.Command{
		Name:  "env",
		Usage: "manage the app's environment variables (stored securely in the cluster)",
		Commands: []*cli.Command{
			{
				Name:      "set",
				Usage:     "set one or more variables, e.g. donkey env set GREETING=hello",
				ArgsUsage: "KEY=VALUE [KEY=VALUE…]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "no-restart", Usage: "don't restart the app now (changes apply on next deploy)"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return envMutate(ctx, cmd, func(env map[string]string) error {
						if cmd.Args().Len() == 0 {
							return fmt.Errorf("tell donkey what to set, e.g. donkey env set GREETING=hello")
						}
						for _, arg := range cmd.Args().Slice() {
							k, v, ok := strings.Cut(arg, "=")
							if !ok || k == "" {
								return fmt.Errorf("%q doesn't look like KEY=VALUE", arg)
							}
							env[k] = v
						}
						return nil
					})
				},
			},
			{
				Name:      "unset",
				Usage:     "remove one or more variables",
				ArgsUsage: "KEY [KEY…]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "no-restart", Usage: "don't restart the app now"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return envMutate(ctx, cmd, func(env map[string]string) error {
						if cmd.Args().Len() == 0 {
							return fmt.Errorf("tell donkey what to remove, e.g. donkey env unset GREETING")
						}
						for _, k := range cmd.Args().Slice() {
							delete(env, k)
						}
						return nil
					})
				},
			},
			{
				Name:  "list",
				Usage: "show the app's variables",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "show-values", Usage: "print the actual values instead of hiding them"},
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
					env, err := api.GetEnv(ctx, app.Name)
					if err != nil {
						return err
					}
					if len(env) == 0 {
						s.UI.Info("%s has no variables yet — add one with `donkey env set KEY=value`.", app.Name)
						return nil
					}
					keys := make([]string, 0, len(env))
					for k := range env {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, k := range keys {
						if cmd.Bool("show-values") {
							s.UI.Printf("%s=%s", k, env[k])
						} else {
							s.UI.Printf("%s=%s", k, "••••")
						}
					}
					return nil
				},
			},
		},
	}
}

// envMutate loads current env via the server, applies mutate, saves, restarts.
func envMutate(ctx context.Context, cmd *cli.Command, mutate func(map[string]string) error) error {
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
	env, err := api.GetEnv(ctx, app.Name)
	if err != nil {
		return err
	}
	if env == nil {
		env = map[string]string{}
	}
	if err := mutate(env); err != nil {
		return err
	}
	restarted, err := api.PutEnv(ctx, app.Name, env, !cmd.Bool("no-restart"))
	if err != nil {
		return fmt.Errorf("couldn't save the variables: %w", err)
	}
	s.UI.Success("Saved — %s now has %d variable(s).", app.Name, len(env))
	switch {
	case restarted:
		s.UI.Success("Restarting %s so it picks up the change…", app.Name)
	case cmd.Bool("no-restart"):
		s.UI.Info("The running app keeps its old values until the next deploy or restart.")
	}
	return nil
}
