//go:build darwin || linux

//nolint:wsl_v5 // PTY lifecycle setup stays sequential and auditable.
package main

import (
	"bytes"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInteractiveSignalRestoresTTYWithoutClosingOutput(t *testing.T) {
	if os.Getenv("PIPS_MAIN_PTY_HELPER") == "1" {
		os.Exit(run(nil, os.Stdin, os.Stdout, os.Stderr))
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
			testInteractiveSignal(t, test.signal, test.code)
		})
	}
}

func testInteractiveSignal(t *testing.T, signal os.Signal, code int) {
	t.Helper()

	executable, err := os.Executable()
	require.NoError(t, err)
	environment := append(os.Environ(),
		"PIPS_MAIN_PTY_HELPER=1",
		"PIPS_HOME="+t.TempDir(),
		"TERM=xterm-256color",
		"NO_COLOR=1",
	)

	terminal, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = terminal.Close()
		_ = slave.Close()
	})
	require.NoError(t, pty.Setsize(terminal, &pty.Winsize{Rows: 24, Cols: 80}))
	process, err := os.StartProcess(
		executable,
		[]string{executable, "-test.run=TestInteractiveSignalRestoresTTYWithoutClosingOutput"},
		&os.ProcAttr{
			Env:   environment,
			Files: []*os.File{slave, slave, slave},
			Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: true},
		},
	)
	require.NoError(t, err)
	require.NoError(t, slave.Close())

	var output bytes.Buffer
	readDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&output, terminal)
		close(readDone)
	}()

	time.Sleep(400 * time.Millisecond)
	require.NoError(t, process.Signal(signal))
	waitDone := make(chan struct {
		state *os.ProcessState
		err   error
	}, 1)
	go func() {
		state, waitErr := process.Wait()
		waitDone <- struct {
			state *os.ProcessState
			err   error
		}{state: state, err: waitErr}
	}()
	select {
	case result := <-waitDone:
		require.NoError(t, result.err)
		assert.Equal(t, code, result.state.ExitCode())
	case <-time.After(10 * time.Second):
		_ = process.Kill()
		t.Fatal("interactive process did not exit after signal")
	}

	_ = terminal.Close()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("PTY reader did not finish")
	}

	value := output.String()
	assert.NotContains(t, value, "\x1b[?1049h")
	assert.Contains(t, value, "\x1b[?1007l")
	assert.NotContains(t, value, "\x1b[?1007h")
}
