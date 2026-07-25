package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/urfave/cli/v3"
)

func openCommand() *cli.Command {
	return &cli.Command{
		Name:  "open",
		Usage: "open this folder's app in your browser",
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
			st, err := api.Status(ctx, s.Space, app.Name)
			if err != nil {
				return err
			}
			if st.URL == "" {
				return fmt.Errorf("%s doesn't have a web address yet — run `donkey deploy` first", app.Name)
			}
			s.UI.Printf("Opening %s …", st.URL)
			return browse(st.URL)
		},
	}
}

// browse opens the URL with the platform's standard opener. This is the one
// place donkey execs another program — there is no library way to reach the
// user's browser.
func browse(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
