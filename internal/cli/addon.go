package cli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"monobox.ch/donkey/internal/config"
	"monobox.ch/donkey/internal/server"
)

func addonCommand() *cli.Command {
	return &cli.Command{
		Name:  "addon",
		Usage: "attach services like databases to your app",
		Commands: []*cli.Command{
			addonAddCommand(),
			addonAttachCommand(),
			addonRemoveCommand(),
			addonListCommand(),
		},
	}
}

func addonAddCommand() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "create a service inside the cluster, e.g. donkey addon add postgres",
		ArgsUsage: "TYPE",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "name", Usage: "name for this addon (defaults to its type)"},
			&cli.StringFlag{Name: "version", Usage: "service version (e.g. 16 for postgres)"},
			&cli.StringFlag{Name: "storage", Usage: "disk size, e.g. 1Gi"},
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
			typ := cmd.Args().First()
			if typ == "" {
				return fmt.Errorf("tell donkey what to add, e.g. donkey addon add postgres")
			}
			name := cmd.String("name")
			if name == "" {
				name = typ
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			u := s.UI
			u.Printf("Setting up %s for %s…", u.Bold(typ), u.Bold(app.Name))
			if err := api.AddAddon(ctx, s.Space, app.Name, server.AddonRequest{
				Type: typ, Name: name, Version: cmd.String("version"), Storage: cmd.String("storage"),
			}); err != nil {
				return err
			}
			u.Success("%s is ready.", typ)
			return recordAddonAndRedeploy(ctx, s, cmd, app, config.AddonConfig{
				Name: name, Type: typ, Version: cmd.String("version"), Storage: cmd.String("storage"),
			})
		},
	}
}

func addonAttachCommand() *cli.Command {
	return &cli.Command{
		Name:      "attach",
		Usage:     "connect a service that already runs elsewhere, e.g. donkey addon attach postgres --url postgres://…",
		ArgsUsage: "TYPE",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "url", Required: true, Usage: "connection address of the existing service"},
			&cli.StringFlag{Name: "name", Usage: "name for this addon (defaults to its type)"},
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
			typ := cmd.Args().First()
			if typ == "" {
				return fmt.Errorf("tell donkey what to attach, e.g. donkey addon attach postgres --url …")
			}
			name := cmd.String("name")
			if name == "" {
				name = typ
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			if err := api.AddAddon(ctx, s.Space, app.Name, server.AddonRequest{
				Type: typ, Name: name, External: true, URL: cmd.String("url"),
			}); err != nil {
				return err
			}
			s.UI.Success("Connected — the app will see this %s exactly like a donkey-managed one.", typ)
			return recordAddonAndRedeploy(ctx, s, cmd, app, config.AddonConfig{
				Name: name, Type: typ, External: true,
			})
		},
	}
}

func addonRemoveCommand() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "remove an addon from the app",
		ArgsUsage: "NAME",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "keep-data", Usage: "keep the stored data (only the running service goes away)"},
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
			name := cmd.Args().First()
			if name == "" {
				return fmt.Errorf("tell donkey which addon to remove — `donkey addon list` shows them")
			}
			idx := -1
			for i, a := range app.Addons {
				if a.Name == name {
					idx = i
				}
			}
			if idx < 0 {
				return fmt.Errorf("%s isn't attached to %s — `donkey addon list` shows what is", name, app.Name)
			}
			entry := app.Addons[idx]
			u := s.UI
			if !entry.External && !cmd.Bool("keep-data") {
				if !u.AssumeYes && !u.Confirm(fmt.Sprintf("Remove %s AND permanently delete its data?", name), false) {
					u.Info("Nothing changed. Use --keep-data to remove only the running service.")
					return nil
				}
			}
			api, err := s.api()
			if err != nil {
				return err
			}
			if err := api.RemoveAddon(ctx, s.Space, app.Name, name, cmd.Bool("keep-data")); err != nil {
				return err
			}
			app.Addons = append(app.Addons[:idx], app.Addons[idx+1:]...)
			if err := app.Save(s.Resolved.Flags.AppDir); err != nil {
				return err
			}
			u.Success("%s removed.", name)
			return redeployForAddons(ctx, s, cmd, app)
		},
	}
}

func addonListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "show the services attached to this app",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			app, err := requireApp(s)
			if err != nil {
				return err
			}
			if len(app.Addons) == 0 {
				s.UI.Info("%s has no addons — add a database with `donkey addon add postgres`.", app.Name)
				return nil
			}
			for _, a := range app.Addons {
				kind := "managed by donkey"
				if a.External {
					kind = "running elsewhere (attached)"
				}
				detail := a.Type
				if a.Version != "" {
					detail += " " + a.Version
				}
				s.UI.Printf("%s — %s, %s", s.UI.Bold(a.Name), detail, kind)
			}
			return nil
		},
	}
}

// recordAddonAndRedeploy persists the addon in donkey.yaml (replacing any
// same-name entry) and redeploys so the app picks up the binding.
func recordAddonAndRedeploy(ctx context.Context, s *session, cmd *cli.Command, app *config.AppConfig, entry config.AddonConfig) error {
	replaced := false
	for i, a := range app.Addons {
		if a.Name == entry.Name {
			app.Addons[i] = entry
			replaced = true
		}
	}
	if !replaced {
		app.Addons = append(app.Addons, entry)
	}
	if err := app.Save(s.Resolved.Flags.AppDir); err != nil {
		return fmt.Errorf("couldn't update donkey.yaml: %w", err)
	}
	s.UI.Detail("recorded in donkey.yaml")
	return redeployForAddons(ctx, s, cmd, app)
}

// redeployForAddons re-runs a deploy (server-side, --no-build isn't needed:
// the app was already deployed, and the wiring lives in its Deployment).
func redeployForAddons(ctx context.Context, s *session, cmd *cli.Command, app *config.AppConfig) error {
	api, err := s.api()
	if err != nil {
		return err
	}
	st, err := api.Status(ctx, s.Space, app.Name)
	if err != nil {
		return err
	}
	if !st.Found || st.Image == "" {
		s.UI.Info("Run %s to put the app live with this change.", s.UI.Bold("donkey deploy"))
		return nil
	}
	s.UI.Info("Restarting %s so it sees the change…", app.Name)
	return deployWithTag(ctx, s, cmd, st.Image)
}
