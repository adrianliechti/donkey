// Package cli wires up the donkey command tree (urfave/cli v3).
package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/gitutil"
	"github.com/adrianliechti/donkey/internal/ui"
)

// version is stamped via -ldflags "-X github.com/adrianliechti/donkey/internal/cli.version=…".
var version = "dev"

func Run(ctx context.Context, args []string) error {
	root := &cli.Command{
		Name:    "donkey",
		Usage:   "deploy your apps to Kubernetes without the hard parts",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "app-dir", Value: ".", Usage: "folder of the app to work on"},
			&cli.StringFlag{Name: "space", Usage: "space the app belongs to (default: the repository's name)"},
			&cli.StringFlag{Name: "context", Usage: "kubeconfig context (only used by `donkey server install`)"},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "show technical detail"},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "answer yes to safe questions (auto-fixes, defaults)"},
		},
		Commands: []*cli.Command{
			initCommand(),
			deployCommand(),
			appsCommand(),
			statusCommand(),
			logsCommand(),
			envCommand(),
			filesCommand(),
			addonCommand(),
			destroyCommand(),
			openCommand(),
			doctorCommand(),
			loginCommand(),
			serverCommand(),
			configCommand(),
		},
		Action: overviewAction,
	}
	return root.Run(ctx, args)
}

// session loads config and builds the shared helpers each command needs.
type session struct {
	UI         *ui.UI
	Resolved   *config.Resolved
	GlobalPath string
	// Space is the app folder's space, already resolved (flag > donkey.yaml >
	// repository name > folder name). Every server call is scoped by it.
	Space string
	// AppErr is a broken donkey.yaml. It must not lock the user out of every
	// command (`donkey doctor` exists to explain exactly this) — commands
	// that need the app surface it via requireApp.
	AppErr error
}

func newSession(cmd *cli.Command) (*session, error) {
	u := ui.New(cmd.Bool("verbose"), cmd.Bool("yes"))
	globalPath, err := config.DefaultGlobalPath()
	if err != nil {
		return nil, fmt.Errorf("donkey couldn't find your home folder: %w", err)
	}
	global, err := config.LoadGlobal(globalPath)
	if err != nil {
		return nil, err
	}
	appDir := cmd.String("app-dir")
	app, appErr := config.LoadApp(appDir)
	flags := config.Flags{
		KubeContext: cmd.String("context"),
		AppDir:      appDir,
		Space:       cmd.String("space"),
	}
	return &session{
		UI:         u,
		Resolved:   config.Resolve(flags, global, app),
		GlobalPath: globalPath,
		Space:      config.ResolveSpace(flags.Space, app, appDir, gitutil.RepoRoot(appDir)),
		AppErr:     appErr,
	}, nil
}

// api returns the server transport, or a friendly error when not logged in.
func (s *session) api() (*apiclient.Client, error) {
	if s.Resolved.Global == nil || s.Resolved.Global.Server.URL == "" {
		return nil, fmt.Errorf("donkey isn't connected to a server yet — run `donkey login --server https://donkey.<your-domain> --token …` (or ask whoever ran `donkey server install` for the address and token)")
	}
	server := s.Resolved.Global.Server
	return apiclient.New(server.URL, server.ResolveToken(), server.CAFile)
}

// overviewAction runs on bare `donkey`: a friendly orientation, not a help dump.
func overviewAction(ctx context.Context, cmd *cli.Command) error {
	s, err := newSession(cmd)
	if err != nil {
		return err
	}
	u := s.UI
	u.Printf("%s", u.Bold("donkey "+version+" — deploy apps to Kubernetes without the hard parts"))
	u.Printf("")
	if s.Resolved.Global == nil || s.Resolved.Global.Server.URL == "" {
		u.Printf("Welcome! This machine isn't connected to a donkey server yet.")
		u.Printf("")
		u.Printf("  · Have a server already? %s connects you.", u.Bold("donkey login"))
		u.Printf("  · Setting one up? %s installs the control plane into your cluster.", u.Bold("donkey server install"))
		return nil
	}
	u.Success("Connected to %s", s.Resolved.Global.Server.URL)
	if s.AppErr != nil {
		u.Printf("")
		u.Warn("This folder's donkey.yaml has a problem — %s explains it.", u.Bold("donkey doctor"))
		return nil
	}
	if s.Resolved.App != nil {
		u.Printf("")
		u.Printf("This folder contains the app %s, in the space %s.", u.Bold(s.Resolved.App.Name), u.Bold(s.Space))
		u.Printf("  %s deploys it · %s shows how it's doing · %s explains any problems.",
			u.Bold("donkey deploy"), u.Bold("donkey status"), u.Bold("donkey doctor"))
	} else {
		abs, _ := filepath.Abs(s.Resolved.Flags.AppDir)
		u.Printf("")
		u.Printf("No app in %s yet — run %s inside a project folder to get started.", abs, u.Bold("donkey deploy"))
	}
	return nil
}
