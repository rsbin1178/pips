//go:build darwin || linux

//nolint:wsl_v5 // PTY lifecycle setup stays sequential and auditable.
package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPTYLifecycleRestoresTerminalBeforeControllerClose(t *testing.T) {
	if os.Getenv("PIPS_TUI_PTY_HELPER") == "1" {
		controller := &ptyController{Controller: openScriptedController(t)}
		_, _ = os.Stdout.WriteString("SHELL_HISTORY_MARKER\n")
		err := Run(context.Background(), Options{
			Input:       os.Stdin,
			Output:      os.Stdout,
			Environment: os.Environ(),
			Workspace:   "/workspace",
			Trusted:     true,
			NoColor:     true,
			Bootstrap: func(context.Context, bool) (Controller, error) {
				return controller, nil
			},
		})
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}

		os.Exit(0)
	}
	t.Parallel()

	executable, err := os.Executable()
	require.NoError(t, err)
	environment := append(os.Environ(),
		"PIPS_TUI_PTY_HELPER=1",
		"TERM=xterm-256color",
		"NO_COLOR=1",
	)

	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = master.Close()
		_ = slave.Close()
	})
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 80}))
	before, err := term.GetState(master.Fd())
	require.NoError(t, err)

	process, err := os.StartProcess(
		executable,
		[]string{executable, "-test.run=TestPTYLifecycleRestoresTerminalBeforeControllerClose"},
		&os.ProcAttr{
			Env:   environment,
			Files: []*os.File{slave, slave, slave},
			Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: true},
		},
	)
	require.NoError(t, err)

	var output synchronizedBuffer
	readDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&output, master)
		close(readDone)
	}()

	waitForPTYOutput(t, &output, "\x1b[?2004h", 5*time.Second)
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Rows: 36, Cols: 100}))
	_, err = master.Write([]byte("\x1b[200~small @ literal\x1b[201~"))
	require.NoError(t, err)
	waitForPTYOutput(t, &output, "small @ literal", 5*time.Second)
	_, err = master.Write([]byte("\x0a"))
	require.NoError(t, err)
	_, err = master.Write([]byte(
		"\x1b[200~paste one\npaste two\npaste three\npaste four\n" +
			"paste five\npaste six\npaste seven\npaste eight\npaste nine\x1b[201~",
	))
	require.NoError(t, err)
	waitForPTYOutput(t, &output, "[Pasted text #1", 5*time.Second)
	_, err = master.Write([]byte("\x0actrl-j\x1b[13;2ushift-enter\r"))
	require.NoError(t, err)
	_, err = master.Write([]byte(
		"\x1b[<64;10;5M" +
			"\x1b[<0;10;5M" +
			"\x1b[<0;10;5m" +
			"\x1b[<32;11;5M",
	))
	require.NoError(t, err)
	waitForPTYOutput(t, &output, "▣ Team Worker · Team task · Completed", 5*time.Second)
	_, err = master.Write([]byte{0x03})
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)
	_, err = master.Write(append([]byte{0x0b}, []byte("quit\r")...))
	require.NoError(t, err)

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
		assert.Equal(t, 0, result.state.ExitCode())
	case <-time.After(10 * time.Second):
		_ = process.Kill()
		t.Fatal("TUI helper did not exit")
	}

	after, err := term.GetState(master.Fd())
	require.NoError(t, err)
	assert.True(t, reflect.DeepEqual(before, after), "terminal mode was not restored")
	require.NoError(t, slave.Close())
	select {
	case <-readDone:
	case <-time.After(time.Second):
		_ = master.Close()
		<-readDone
	}

	value := output.String()
	reset := strings.LastIndex(value, resetTerminalInteraction)
	closed := strings.LastIndex(value, "CONTROLLER_CLOSED")
	assert.NotContains(t, value, "\x1b[?1049h", "alternate screen must remain disabled")
	assert.NotContains(t, value, "\x1b[?1007h", "alternate scroll must remain disabled")
	assert.NotContains(t, value, "\x1b[?1002h", "cell mouse reporting must remain disabled")
	assert.NotContains(t, value, "\x1b[?1003h", "all-motion mouse reporting must remain disabled")
	assert.GreaterOrEqual(t, reset, 0, "terminal interaction modes were not reset")
	assert.Greater(t, closed, reset, "controller closed before terminal restoration")
	assert.Contains(t, value, "SHELL_HISTORY_MARKER")
	assert.Contains(t, value, "Pips")
	assert.Contains(t, value, "✻ Pips")
	assert.Contains(t, value, "small @ literal")
	assert.Contains(t, value, "[Pasted text #1")
	assert.Contains(t, value, "PROMPT_LINES=12")
	assert.Contains(t, value, "scripted final answer")
	assert.Contains(t, value, "▣ openai/tui-scripted ·")
	assert.Contains(t, value, "▣ Team Worker · Team task · Completed")
	for _, private := range []string{
		"team-pty", "member-pty", "task-pty", "attempt-pty", "child-pty", "private_ref",
	} {
		assert.NotContains(t, value, private)
	}
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

func waitForPTYOutput(
	t *testing.T,
	output *synchronizedBuffer,
	text string,
	timeout time.Duration,
) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), text) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("PTY output did not contain %q before deadline", text)
}

type ptyController struct {
	Controller

	mu          sync.Mutex
	promptLines int
}

func (c *ptyController) Prompt(
	ctx context.Context,
	messages ...ai.Message,
) iter.Seq2[coding.Event, error] {
	lines := 0
	if len(messages) > 0 {
		lines = strings.Count(visibleMessageText(messages[0]), "\n") + 1
	}

	c.mu.Lock()
	c.promptLines = lines
	c.mu.Unlock()

	upstream := c.Controller.Prompt(ctx, messages...)

	return func(yield func(coding.Event, error) bool) {
		var latest coding.Event
		for event, eventErr := range upstream {
			if eventErr == nil {
				latest = event
			}
			if !yield(event, eventErr) || eventErr != nil {
				return
			}
		}
		if latest.SessionID == "" {
			return
		}

		attempt := coding.TeamLifecycle{
			TeamID: team.ID("team-pty"), MemberID: team.MemberID("member-pty"),
			TaskID: team.TaskID("task-pty"), AttemptID: team.AttemptID("attempt-pty"),
			ChildSessionID: "child-pty", State: coding.TeamLifecycleRunning,
			Activity: coding.TeamActivityWorking,
		}
		running := coding.Event{
			Schema: coding.EventSchema, Sequence: latest.Sequence + 1,
			Time: latest.Time.Add(time.Millisecond), SessionID: latest.SessionID,
			Type: coding.EventTeamLifecycle, Payload: attempt,
		}
		if !yield(running, nil) {
			return
		}

		attempt.State = coding.TeamLifecycleCompleted
		attempt.Activity = ""
		attempt.Code = "private_ref"
		attempt.Turns = 2
		attempt.ToolCalls = 3
		attempt.Usage = coding.TokenUsage{InputTokens: 120, OutputTokens: 80}
		attempt.DurationMillis = 2_000
		_ = yield(coding.Event{
			Schema: coding.EventSchema, Sequence: latest.Sequence + 2,
			Time: latest.Time.Add(2 * time.Millisecond), SessionID: latest.SessionID,
			Type: coding.EventTeamLifecycle, Payload: attempt,
		}, nil)
	}
}

func (c *ptyController) Close(ctx context.Context) error {
	closeErr := c.Controller.Close(ctx)
	c.mu.Lock()
	lines := c.promptLines
	c.mu.Unlock()

	_, markerErr := fmt.Fprintf(os.Stdout, "PROMPT_LINES=%d;CONTROLLER_CLOSED", lines)

	return errors.Join(closeErr, markerErr)
}
