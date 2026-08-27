// Package cli exposes donkey's CLI as a command that can be embedded in other
// urfave/cli applications.
package cli

import (
	"context"

	urfavecli "github.com/urfave/cli/v3"

	internalcli "github.com/adrianliechti/donkey/internal/cli"
)

// RunOptions controls which donkey capabilities an embedding CLI exposes.
type RunOptions struct {
	// DisableManagement omits commands and flags that install or manage the
	// donkey control plane. Commands that use an existing server remain
	// available.
	DisableManagement bool
	// Server provides the default donkey-server URL when the user has not
	// selected and saved one. An explicit login --server overrides it.
	Server string
}

// Command returns donkey's command tree for attachment to another urfave/cli
// application.
func Command(options RunOptions) *urfavecli.Command {
	return internalcli.Command(internalcli.Options{
		DisableManagement: options.DisableManagement,
		Server:            options.Server,
	})
}

// Run executes donkey as a standalone CLI.
func Run(ctx context.Context, args []string, options RunOptions) error {
	return Command(options).Run(ctx, args)
}
