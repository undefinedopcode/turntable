// Command octoparser queries heterogeneous data sources with an SQL-style
// language. See DESIGN.md for the architecture and supported dialect.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/april/octoparser/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	app := cli.NewApp()
	os.Exit(app.Run(ctx, os.Args[1:]))
}