// Command pips runs the terminal-first coding agent application.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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
	interruptCtx, stopInterrupt := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopInterrupt()

	commandCtx, stopTermination := signal.NotifyContext(interruptCtx, syscall.SIGTERM)
	defer stopTermination()

	sigpipe := make(chan os.Signal, 1)

	signal.Notify(sigpipe, syscall.SIGPIPE)
	defer signal.Stop(sigpipe)

	ioDone := make(chan struct{})

	ioStopped := make(chan struct{})
	go func() {
		defer close(ioStopped)

		select {
		case <-commandCtx.Done():
			_ = stdout.Close()
		case <-ioDone:
		}
	}()

	defer func() {
		close(ioDone)
		<-ioStopped
	}()

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
