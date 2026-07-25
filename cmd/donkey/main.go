package main

import (
	"context"
	"fmt"
	"os"

	"github.com/adrianliechti/donkey/internal/cli"
)

func main() {
	if err := cli.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "\n%s\n", err)
		os.Exit(1)
	}
}
