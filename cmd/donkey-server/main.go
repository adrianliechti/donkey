package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/adrianliechti/donkey/internal/gateway"
	"github.com/adrianliechti/donkey/internal/server"
)

// One binary, two roles: the control plane by default, the login gateway
// with `gateway` as the first argument. Same image, second Deployment.
func main() {
	// SIGTERM starts a drain rather than killing in-flight work: a
	// control-plane upgrade must not abandon somebody's half-finished build,
	// and the gateway finishes the responses it is already streaming.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if len(os.Args) > 1 && os.Args[1] == "gateway" {
		cfg, err := gateway.FromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "configuration error:", err)
			os.Exit(1)
		}
		gw, err := gateway.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "startup error:", err)
			os.Exit(1)
		}
		if err := gw.Run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	cfg, err := server.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(1)
	}
	srv, err := server.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup error:", err)
		os.Exit(1)
	}
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
