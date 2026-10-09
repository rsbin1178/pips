//go:build darwin || linux

//nolint:wsl_v5 // PTY lifecycle setup stays sequential and auditable.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	searchPTYWidth   = 100
	searchPTYHeight  = 24
	searchPTYMarkers = 200
)

// searchPTYController adds the child-Agent list the wheel step needs to the
// scripted state.
type searchPTYController struct {
	stubController

	agents []subagent.Summary
}

func (c searchPTYController) ListSubagents(context.Context) ([]subagent.Summary, error) {
	return c.agents, nil
}

// TestPTYSearchAndListWheel drives the real Program through this change: Ctrl+F
// must find and scroll to a match in the managed viewport, the Ctrl+T detail panel
// must move with the wheel, and the Agent list must scroll its viewport the same
// way.
func TestPTYSearchAndListWheel(t *testing.T) {
	if os.Getenv("PIPS_TUI_SEARCH_PTY_HELPER") == "1" {
		state := readyState()
		for index := range searchPTYMarkers {
			state.Transcript = append(
				state.Transcript,
				ai.AssistantText(fmt.Sprintf("MARKER-%03d", index)),
			)
		}
		state.Tools = []coding.ToolState{{
			Call: coding.ToolCall{
				ID: "call-1", Name: "shell",
				Arguments: ai.JSON(`{"command":"seq 1 60"}`),
			},
			Status: coding.ToolStatusCompleted,
			Result: ai.ToolResultText(
				"call-1", "shell",
				"stdout:\n"+strings.Repeat("output line\n", 60),
			),
		}}

		controller := searchPTYController{stubController: stubController{state: state}}
		for index := range 14 {
			controller.agents = append(controller.agents, subagent.Summary{
				ChildSessionID: fmt.Sprintf("child-%02d", index),
				Role:           subagent.RoleExplore,
				State:          subagent.StateSucceeded,
				TaskPreview:    fmt.Sprintf("Inspect package %02d", index),
				Model:          "openai/model",
				Code:           "ok",
			})
		}

		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
			MouseReporting: true,
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

	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close(); _ = master.Close() })
	require.NoError(t, pty.Setsize(master, &pty.Winsize{
		Rows: searchPTYHeight, Cols: searchPTYWidth,
	}))
	before, err := term.GetState(master.Fd())
	require.NoError(t, err)

	process, err := os.StartProcess(executable,
		[]string{executable, "-test.run=^TestPTYSearchAndListWheel$"},
		&os.ProcAttr{
			Env: append(os.Environ(), "PIPS_TUI_SEARCH_PTY_HELPER=1",
				"TERM=xterm-256color", "NO_COLOR=1"),
			Files: []*os.File{slave, slave, slave},
			Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: true},
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = process.Kill() })

	var output synchronizedBuffer

	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(&output, master); close(readDone) }()

	frame := func() string { return searchPTYFrame(t, &output) }

	require.Eventually(t, func() bool {
		onScreen := frame()

		return strings.Contains(onScreen, "MARKER-199") && strings.Contains(onScreen, "idle")
	}, 30*time.Second, 10*time.Millisecond)

	// Ctrl+F, then a marker far above the tail: the jump has to scroll to it.
	_, err = master.Write([]byte{0x06})
	require.NoError(t, err)
	_, err = master.Write([]byte("MARKER-035"))
	require.NoError(t, err)
	_, err = master.Write([]byte{'\r'})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		onScreen := frame()

		return strings.Contains(onScreen, "MARKER-035") &&
			strings.Contains(onScreen, "find · 1/1")
	}, 30*time.Second, 10*time.Millisecond)

	scrolled := frame()
	assert.NotContains(t, scrolled, "MARKER-199", "the jump left the tail")
	assert.Contains(t, scrolled, bandScrollIcon,
		"the reserved band tells the paused reader where the newest rows are")

	// Escape closes the box and hands the composer band back.
	_, err = master.Write([]byte{0x1b})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return !strings.Contains(frame(), "find ·")
	}, 30*time.Second, 10*time.Millisecond)

	// Ctrl+T opens the Tool detail panel over the same conversation.
	_, err = master.Write([]byte{0x14})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(frame(), "Ctrl+T/Esc close")
	}, 30*time.Second, 10*time.Millisecond)

	start, total := searchPTYPosition(t, frame())
	require.Positive(t, total, "the detail panel must have a scrollable document")
	require.Equal(t, 1, start, "the panel opens at its first row")

	// One wheel notch moves the panel by the same delta its keys use.
	_, err = master.Write([]byte("\x1b[<65;10;5M"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		moved, _ := searchPTYPosition(t, frame())

		return moved == 1+wheelLinesDefault
	}, 30*time.Second, 10*time.Millisecond)

	// Escape returns to the conversation, and the session still quits cleanly.
	_, err = master.Write([]byte{0x1b})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return !strings.Contains(frame(), "Ctrl+T/Esc close")
	}, 30*time.Second, 10*time.Millisecond)

	// The Agent list is the other list shape: it has its own viewport, so the wheel
	// scrolls it and leaves the selection where it is.
	_, err = master.Write([]byte("\x0bagents\r"))
	require.NoError(t, err)
	// Wait for the list body, not for its title. The title renders as soon as the
	// route is entered, while the rows arrive with the Agent data and the frame
	// shows a loading placeholder until they do; asserting on the title alone can
	// parse a frame whose rows and overflow label are not there yet.
	require.Eventually(t, func() bool {
		return strings.Contains(frame(), "Agents · Runs") && searchPTYLinesPattern.MatchString(frame())
	}, 30*time.Second, 10*time.Millisecond)

	listStart, listTotal := searchPTYOverflowLines(t, frame())
	require.Equal(t, 1, listStart, "the list opens at its first row")
	require.Greater(t, listTotal, searchPTYHeight, "the list must overflow the window")

	_, err = master.Write([]byte("\x1b[<65;10;5M"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		moved, _ := searchPTYOverflowLines(t, frame())

		return moved == 1+wheelLinesDefault
	}, 30*time.Second, 10*time.Millisecond)

	_, err = master.Write([]byte{0x1b})
	require.NoError(t, err)
	// Wait for the ready view, not for the route title: the wheel scrolled the title
	// out of the window, so only the composer band and the status line prove the
	// route handed the screen back. The wait also separates the Esc byte from the
	// next write, so the two can never be read as one Alt sequence.
	require.Eventually(t, func() bool {
		onScreen := frame()

		return strings.Contains(onScreen, "idle") && strings.Contains(onScreen, "❯")
	}, 30*time.Second, 10*time.Millisecond)

	_, err = master.Write([]byte("\x0bquit\r"))
	require.NoError(t, err)

	finished := make(chan *os.ProcessState, 1)
	go func() { state, _ := process.Wait(); finished <- state }()

	var result *os.ProcessState
	require.Eventually(t, func() bool {
		select {
		case result = <-finished:
			return true
		default:
			return false
		}
	}, 30*time.Second, 10*time.Millisecond)
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", output.String())

	after, err := term.GetState(master.Fd())
	require.NoError(t, err)
	assert.Equal(t, before, after, "terminal modes restored")
	_ = slave.Close()
	_ = master.Close()
	<-readDone
}

// searchPTYFrame replays everything the Program wrote and returns the current
// screen, exactly as a terminal would show it.
func searchPTYFrame(t *testing.T, output *synchronizedBuffer) string {
	t.Helper()

	e := newScreenReplay(searchPTYWidth, searchPTYHeight)
	_, err := e.Write([]byte(output.String()))
	require.NoError(t, err)

	frame := e.String()
	_ = e.Close()

	return ansi.Strip(frame)
}

var (
	// searchPTYPositionPattern matches the detail panel's "start-end/total" label.
	searchPTYPositionPattern = regexp.MustCompile(`(\d+)-(\d+)/(\d+)`)
	// searchPTYLinesPattern matches the "… lines start-end/total …" label a
	// scrollable route body appends when its content is taller than the window.
	searchPTYLinesPattern = regexp.MustCompile(`lines (\d+)-(\d+)/(\d+)`)
)

// searchPTYPosition parses the detail panel's "start-end/total" footer label.
func searchPTYPosition(t *testing.T, frame string) (int, int) {
	t.Helper()

	return searchPTYLabel(t, searchPTYPositionPattern, frame)
}

// searchPTYOverflowLines parses the overflow label a scrollable route body shows.
func searchPTYOverflowLines(t *testing.T, frame string) (int, int) {
	t.Helper()

	return searchPTYLabel(t, searchPTYLinesPattern, frame)
}

func searchPTYLabel(t *testing.T, pattern *regexp.Regexp, frame string) (int, int) {
	t.Helper()

	match := pattern.FindStringSubmatch(frame)
	require.Len(t, match, 4, "no position label in frame: %q", frame)

	start, err := strconv.Atoi(match[1])
	require.NoError(t, err)

	total, err := strconv.Atoi(match[3])
	require.NoError(t, err)

	return start, total
}
