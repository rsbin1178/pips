package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnlyContextCanceled(t *testing.T) {
	t.Parallel()

	assert.True(t, onlyContextCanceled(errors.Join(context.Canceled, context.Canceled)))
	assert.False(t, onlyContextCanceled(errors.Join(context.Canceled, errors.New("close failed"))))
	assert.False(t, onlyContextCanceled(context.DeadlineExceeded))
}

func TestSignalExitCodesWhileReadingStdin(t *testing.T) {
	if os.Getenv("PIPS_SIGNAL_HELPER") == "1" {
		os.Exit(run([]string{"exec", "-"}, os.Stdin, os.Stdout, os.Stderr))
	}

	t.Parallel()

	tests := []struct {
		name   string
		signal os.Signal
		code   int
	}{
		{name: "interrupt", signal: os.Interrupt, code: 130},
		{name: "terminate", signal: syscall.SIGTERM, code: 143},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			executable, err := os.Executable()
			require.NoError(t, err)
			readEnd, writeEnd, err := os.Pipe()
			require.NoError(t, err)
			process, err := os.StartProcess(executable, []string{
				executable, "-test.run=TestSignalExitCodesWhileReadingStdin",
			}, &os.ProcAttr{
				Env:   append(os.Environ(), "PIPS_SIGNAL_HELPER=1"),
				Files: []*os.File{readEnd, os.Stdout, os.Stderr},
			})
			require.NoError(t, err)
			require.NoError(t, readEnd.Close())

			var processState *os.ProcessState

			t.Cleanup(func() {
				_ = writeEnd.Close()

				if processState == nil {
					_ = process.Kill()
				}
			})
			time.Sleep(150 * time.Millisecond)
			require.NoError(t, process.Signal(test.signal))

			processState, err = process.Wait()
			require.NoError(t, err)
			assert.Equal(t, test.code, processState.ExitCode())
		})
	}
}

func TestBrokenStdoutPipeReturnsFailureInsteadOfSIGPIPE(t *testing.T) {
	if os.Getenv("PIPS_SIGPIPE_HELPER") == "1" {
		os.Exit(run([]string{"version"}, os.Stdin, os.Stdout, os.Stderr))
	}

	t.Parallel()

	executable, err := os.Executable()
	require.NoError(t, err)

	readEnd, writeEnd, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, readEnd.Close())

	process, err := os.StartProcess(executable, []string{
		executable, "-test.run=TestBrokenStdoutPipeReturnsFailureInsteadOfSIGPIPE",
	}, &os.ProcAttr{
		Env:   append(os.Environ(), "PIPS_SIGPIPE_HELPER=1"),
		Files: []*os.File{os.Stdin, writeEnd, os.Stderr},
	})
	require.NoError(t, err)
	require.NoError(t, writeEnd.Close())

	processState, err := process.Wait()
	require.NoError(t, err)
	assert.Equal(t, 1, processState.ExitCode())
}
