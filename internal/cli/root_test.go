package cli

import (
	"context"
	"testing"

	urfavecli "github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestCommandIncludesManagementByDefault(t *testing.T) {
	cmd := Command(Options{})
	if !hasCommand(cmd, "server") {
		t.Fatal("default command tree is missing server management")
	}
	server := findCommand(cmd, "server")
	if !hasCommand(server, "init") || !hasCommand(server, "render") || !hasCommand(server, "install") {
		t.Fatal("server management is missing init, render, or install")
	}
	if !hasFlag(cmd, "context") {
		t.Fatal("default command tree is missing the server context flag")
	}
	if !hasFlag(findCommand(cmd, "login"), "server") {
		t.Fatal("default login command is missing the server flag")
	}
}

func TestCommandCanDisableManagement(t *testing.T) {
	cmd := Command(Options{DisableManagement: true})
	if hasCommand(cmd, "server") {
		t.Fatal("server command is present when management is disabled")
	}
	if hasFlag(cmd, "context") {
		t.Fatal("server context flag is present when management is disabled")
	}
	if !hasCommand(cmd, "deploy") || !hasCommand(cmd, "login") {
		t.Fatal("client commands are missing when management is disabled")
	}
}

func TestCommandCanDefaultServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := Command(Options{Server: "https://donkey.example.com/"})
	login := findCommand(root, "login")
	if !hasFlag(login, "server") {
		t.Fatal("login server flag is missing when a default server is set")
	}

	status := findCommand(root, "status")
	var got string
	status.Action = func(_ context.Context, cmd *urfavecli.Command) error {
		s, err := newSession(cmd)
		if err != nil {
			return err
		}
		got = s.Server.URL
		return nil
	}
	if err := root.Run(context.Background(), []string{"donkey", "--app-dir", home, "status"}); err != nil {
		t.Fatal(err)
	}
	if got != "https://donkey.example.com" {
		t.Fatalf("resolved server = %q, want default server", got)
	}
}

func TestSavedServerOverridesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalPath, err := config.DefaultGlobalPath()
	if err != nil {
		t.Fatal(err)
	}
	global := &config.GlobalConfig{Server: config.ServerConfig{URL: "https://saved.example.com"}}
	if err := global.Save(globalPath); err != nil {
		t.Fatal(err)
	}

	root := Command(Options{Server: "https://default.example.com"})
	status := findCommand(root, "status")
	var got string
	status.Action = func(_ context.Context, cmd *urfavecli.Command) error {
		s, err := newSession(cmd)
		if err != nil {
			return err
		}
		got = s.Server.URL
		return nil
	}
	if err := root.Run(context.Background(), []string{"donkey", "--app-dir", home, "status"}); err != nil {
		t.Fatal(err)
	}
	if got != "https://saved.example.com" {
		t.Fatalf("resolved server = %q, want saved server", got)
	}
}

func TestLoginServerFlagOverridesDefault(t *testing.T) {
	root := Command(Options{Server: "https://default.example.com"})
	login := findCommand(root, "login")
	var got string
	login.Action = func(_ context.Context, cmd *urfavecli.Command) error {
		if !cmd.IsSet("server") {
			t.Fatal("server flag was not marked as explicitly set")
		}
		got = cmd.String("server")
		return nil
	}
	if err := root.Run(context.Background(), []string{"donkey", "login", "--server", "https://override.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got != "https://override.example.com" {
		t.Fatalf("server flag = %q, want explicit override", got)
	}
}

func hasCommand(root *urfavecli.Command, name string) bool {
	return findCommand(root, name) != nil
}

func findCommand(root *urfavecli.Command, name string) *urfavecli.Command {
	for _, cmd := range root.Commands {
		if cmd.Name == name {
			return cmd
		}
	}
	return nil
}

func hasFlag(cmd *urfavecli.Command, name string) bool {
	if cmd == nil {
		return false
	}
	for _, flag := range cmd.Flags {
		for _, candidate := range flag.Names() {
			if candidate == name {
				return true
			}
		}
	}
	return false
}
