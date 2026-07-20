// Command pips runs the terminal-first coding agent application.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rsbin/pips/internal/coding/frontend/cli"
	"github.com/rsbin/pips/internal/coding/paths"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	layout, err := paths.Default()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)

		return 1
	}

	command, err := cli.New(cli.Dependencies{
		Build: cli.BuildInfo{Version: version, Commit: commit, Date: date},
		Paths: layout,
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)

		return 1
	}

	command.SetOut(os.Stdout)
	command.SetErr(os.Stderr)

	if err := command.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)

		return 1
	}

	return 0
}
