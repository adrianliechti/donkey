package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/ui"
	"github.com/adrianliechti/donkey/internal/webpage"
)

func loginCommand(options Options) *cli.Command {
	flags := []cli.Flag{
		&cli.StringFlag{Name: "server", Value: options.Server, Usage: "server address, e.g. https://example.com"},
		&cli.StringFlag{Name: "token", Usage: "use an existing donkey token instead of the browser (automation)"},
		&cli.StringFlag{Name: "ca-file", Usage: "PEM file of the certificate authority to trust (private-CA installations)"},
	}
	return &cli.Command{
		Name:  "login",
		Usage: "connect this machine to a donkey server (opens your browser)",
		Flags: flags,
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

	url := cfg.Server.URL
	if url == "" {
		url = commandOptions(cmd).Server
	}
	if cmd.IsSet("server") {
		url = cmd.String("server")
	}
	token := cmd.String("token")
	caFile := cmd.String("ca-file")
	if caFile == "" {
		caFile = cfg.Server.CAFile
	}

	if url == "" && u.Interactive {
		form := huh.NewForm(huh.NewGroup(
			huh.NewInput().
				Title("Address of your donkey server").
				Description("Example: https://example.com — ask whoever set it up.").
				Value(&url).
				Validate(func(v string) error {
					if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
						return fmt.Errorf("start with http:// or https://")
					}
					return nil
				}),
		))
		if err := form.Run(); err != nil {
			return fmt.Errorf("login was cancelled")
		}
	}
	if url == "" {
		return fmt.Errorf("donkey needs --server to connect")
	}
	url = strings.TrimSuffix(url, "/")

	// The browser is the credential: the gateway at the server runs the OIDC
	// round trip and hands a sealed token to a listener on the loopback. The
	// terminal never sees a password — only the finished token.
	if token == "" {
		if token, err = browserLogin(ctx, u, url); err != nil {
			return err
		}
	}

	u.Info("Checking the connection…")
	client, err := apiclient.New(url, token, caFile)
	if err != nil {
		return err
	}
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("that didn't work: %w", err)
	}
	cfg.Server = config.ServerConfig{URL: url, Token: token, CAFile: caFile}
	if err := cfg.Save(globalPath); err != nil {
		return err
	}
	u.Success("Connected to %s — you're ready to %s.", u.Bold(cfg.Server.URL), u.Bold("donkey deploy"))
	return nil
}

// browserLogin opens the sign-in in the person's browser and waits on the
// loopback for the token the gateway's callback hands over.
func browserLogin(ctx context.Context, u *ui.UI, serverURL string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("couldn't open a local port for the sign-in: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	loginURL := serverURL + "/oauth2/cli?port=" + strconv.Itoa(port)

	tokens := make(chan string, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.URL.Query().Get("token")
			if r.URL.Path != "/callback" || tok == "" {
				http.NotFound(w, r)
				return
			}
			webpage.WriteNotice(w, http.StatusOK, webpage.Notice{Title: "Signed in", Detail: "Close this tab and return to the terminal.", Foot: "donkey cli"})
			select {
			case tokens <- tok:
			default:
			}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	u.Info("Opening your browser to sign in…")
	u.Printf("  %s", u.Faint(loginURL+"   (open it yourself if nothing happens)"))
	_ = browse(loginURL)

	select {
	case tok := <-tokens:
		return tok, nil
	case <-time.After(3 * time.Minute):
		return "", fmt.Errorf("the sign-in didn't complete within 3 minutes — run `donkey login` again")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
