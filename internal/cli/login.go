package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/urfave/cli/v3"

	"monobox.ch/donkey/internal/apiclient"
	"monobox.ch/donkey/internal/config"
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
	globalPath, err := config.DefaultGlobalPath()
	if err != nil {
		return err
	}
	cfg, err := config.LoadGlobal(globalPath)
	if err != nil || cfg == nil {
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

	if (url == "" || token == "") && interactive() {
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

	fmt.Println("Checking the connection…")
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
	fmt.Printf("✓ Connected to %s — you're ready to `donkey deploy`.\n", cfg.Server.URL)
	return nil
}
