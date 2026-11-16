package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/machine"
)

// machineCommand is what a personal machine's image starts with. It is not
// something to run on a laptop — nobody types it — so it stays out of the
// help: the console starts and stops machines.
func machineCommand() *cli.Command {
	return &cli.Command{
		Name:   "machine",
		Usage:  "run inside a personal machine (the machine image's entry point)",
		Hidden: true,
		Commands: []*cli.Command{{
			Name:      "serve",
			Usage:     "prepare the home, guard the front door and run the coding agent in the folder that is opened",
			ArgsUsage: "[-- agent command…]",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "listen", Usage: "address of the front door (default :8080)"},
				&cli.StringFlag{Name: "upstream", Usage: "loopback address the agent listens on (default http://127.0.0.1:9000)"},
				&cli.StringFlag{Name: "workspace", Usage: "where the machine's folders live (default /data/workspace)"},
			},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				// The runtime stops a machine with SIGTERM; the agent gets
				// to finish what it is writing.
				ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
				defer stop()
				return machine.Serve(ctx, machine.Options{
					Listen:    cmd.String("listen"),
					Upstream:  cmd.String("upstream"),
					Workspace: cmd.String("workspace"),
					Command:   cmd.Args().Slice(),
				})
			},
		}},
	}
}
