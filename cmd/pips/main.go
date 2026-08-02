// Command pips runs the terminal-first coding agent application.
//
//nolint:wsl_v5 // Process lifecycle steps stay grouped by ownership.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	term "github.com/charmbracelet/x/term"
	"github.com/rsbin/pips/internal/coding/cli"
	"github.com/rsbin/pips/internal/coding/paths"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin, stdout, stderr *os.File) int {
	return runAfterSignalSetup(args, stdin, stdout, stderr, nil)
}

func runAfterSignalSetup(
	args []string,
	stdin, stdout, stderr *os.File,
	ready func(),
) int {
	interruptCtx, stopInterrupt := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopInterrupt()

	commandCtx, stopTermination := signal.NotifyContext(
		interruptCtx,
		syscall.SIGTERM,
		syscall.SIGHUP,
	)
	defer stopTermination()
	if ready != nil {
		ready()
	}

	sigpipe := make(chan os.Signal, 1)

	signal.Notify(sigpipe, syscall.SIGPIPE)
	defer signal.Stop(sigpipe)

	stopOutputWatcher := watchPipeOutput(commandCtx, stdout)
	defer stopOutputWatcher()

	layout, err := paths.Default()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return cli.ExitCode(err)
	}

	command, err := cli.New(cli.Dependencies{
		Build: cli.BuildInfo{Version: version, Commit: commit, Date: date},
		Paths: layout,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return cli.ExitCode(err)
	}

	command.SetIn(stdin)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs(args)

	err = command.ExecuteContext(commandCtx)

	if interruptCtx.Err() != nil {
		return cli.ExitInterrupted
	}

	if commandCtx.Err() != nil {
		return cli.ExitTerminated
	}

	if err != nil {
		if !onlyContextCanceled(err) {
			_, _ = fmt.Fprintln(stderr, err)
		}

		return cli.ExitCode(err)
	}

	return cli.ExitSuccess
}

func watchPipeOutput(ctx context.Context, output *os.File) func() {
	if output == nil || term.IsTerminal(output.Fd()) {
		return func() {}
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)

		select {
		case <-ctx.Done():
			_ = output.Close()
		case <-done:
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

func onlyContextCanceled(err error) bool {
	if err == nil {
		return false
	}

	type multiUnwrapper interface{ Unwrap() []error }
	if joined, ok := err.(multiUnwrapper); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}

		for _, child := range children {
			if !onlyContextCanceled(child) {
				return false
			}
		}

		return true
	}

	type unwrapper interface{ Unwrap() error }
	if wrapped, ok := err.(unwrapper); ok {
		return onlyContextCanceled(wrapped.Unwrap())
	}

	return errors.Is(err, context.Canceled)
}
