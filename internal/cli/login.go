package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/ui"
)

func loginCommand() *cli.Command {
	return &cli.Command{
		Name:  "login",
		Usage: "connect this machine to a donkey server",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server", Usage: "server address, e.g. https://donkey.apps.example.com"},
			&cli.StringFlag{Name: "token", Usage: "API token (from `donkey server install`)"},
			&cli.StringFlag{Name: "ca-file", Usage: "PEM file of the certificate authority to trust (private-CA installations)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runLogin(ctx, cmd)
		},
	}
}

func runLogin(ctx context.Context, cmd *cli.Command) error {
	u := ui.New(cmd.Bool("verbose"), cmd.Bool("yes"))
	globalPath, err := config.DefaultGlobalPath()
	if err != nil {
		return err
	}
	cfg, err := config.LoadGlobal(globalPath)
	if err != nil {
		// Overwriting a broken file would silently lose whatever else it
		// held (kubeContext, CA path) — make the user look at it first.
		return fmt.Errorf("%w\n\nFix or delete that file, then log in again", err)
	}
	if cfg == nil {
		cfg = &config.GlobalConfig{}
	}

	url := cmd.String("server")
	token := cmd.String("token")
	caFile := cmd.String("ca-file")
	if url == "" {
		url = cfg.Server.URL
	}
	if caFile == "" {
		caFile = cfg.Server.CAFile
	}

	if (url == "" || token == "") && u.Interactive {
		form := huh.NewForm(huh.NewGroup(
			huh.NewInput().
				Title("Address of your donkey server").
				Description("Example: http://donkey.apps.example.com — ask whoever set it up.").
				Value(&url).
				Validate(func(v string) error {
					if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
						return fmt.Errorf("start with http:// or https://")
					}
					return nil
				}),
			huh.NewInput().
				Title("API token").
				Description("Printed by `donkey server install`.").
				EchoMode(huh.EchoModePassword).
				Value(&token),
		))
		if err := form.Run(); err != nil {
			return fmt.Errorf("login was cancelled")
		}
	}
	if url == "" || token == "" {
		return fmt.Errorf("donkey needs both --server and --token to connect")
	}

	u.Info("Checking the connection…")
	client, err := apiclient.New(url, token, caFile)
	if err != nil {
		return err
	}
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("that didn't work: %w", err)
	}
	cfg.Server = config.ServerConfig{URL: strings.TrimSuffix(url, "/"), Token: token, CAFile: caFile}
	if err := cfg.Save(globalPath); err != nil {
		return err
	}
	u.Success("Connected to %s — you're ready to %s.", u.Bold(cfg.Server.URL), u.Bold("donkey deploy"))
	return nil
}
