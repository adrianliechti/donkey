package main

import (
	"fmt"
	"os"

	"monobox.ch/donkey/internal/server"
)

func main() {
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
	if err := srv.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
