//go:build darwin || linux

//nolint:wsl_v5 // PTY setup, observation and teardown follow physical order.
package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPTYThinkingBlocksRenderInTheViewport is the end-to-end proof: a real
// terminal shows visible reasoning as an always-visible Thinking block in the
// fullscreen viewport, and the shipped exit policy leaves the conversation in
// the viewport's store instead of the main screen.
func TestPTYThinkingBlocksRenderInTheViewport(t *testing.T) {
	const testName = "TestPTYThinkingBlocksRenderInTheViewport"
	if os.Getenv("PIPS_TUI_THINKING_PTY_HELPER") == "1" {
		state := readyState()
		state.Transcript = []ai.Message{
			ai.UserText("QUESTION-MARKER"),
			ai.Assistant(
				ai.ReasoningPart{Text: "step one\n\nstep two\n\nstep three\n\nREASON-TAIL"},
				ai.Text("ANSWER-MARKER"),
			),
		}
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
			ExitOutput: config.ExitOutputResumeHint,
			Bootstrap: func(context.Context, bool) (Controller, error) {
				return stubController{state: state}, nil
			},
		})
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	t.Parallel()

	h := newPTYHarness(t, "PIPS_TUI_THINKING_PTY_HELPER", testName, 20, 60)

	require.Eventually(t, func() bool {
		return h.screenMatches(60, 20, func(e *vt.Emulator) bool {
			frame := e.String()

			return e.IsAltScreen() && strings.Contains(frame, thinkingGlyph) &&
				strings.Contains(frame, "step one") &&
				strings.Contains(frame, "REASON-TAIL") &&
				strings.Contains(frame, "ANSWER-MARKER")
		})
	}, 30*time.Second, 10*time.Millisecond)

	// Every reasoning row is on screen; the block never folds away text.
	frame := h.frame(60, 20)
	assert.Contains(t, frame, "step one")
	assert.Contains(t, frame, "step three")
	assert.Contains(t, frame, "REASON-TAIL")

	h.write("\x0bquit\r")
	result := h.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", h.output.String())
	h.assertRestored()

	// The exit handoff runs after restoration, so the restored screen is only
	// observable once the process has finished.
	emulator := newScreenReplay(60, 20)
	_, err := emulator.Write([]byte(h.output.String()))
	require.NoError(t, err)

	if !assert.False(t, emulator.IsAltScreen(), "the alternate screen is restored") {
		reportStrandedAltScreen(t, h.output.String())
	}

	history := stableScreenHistory(emulator)
	assert.NotContains(t, history, "ANSWER-MARKER")
	assert.NotContains(t, history, "REASON-TAIL")
	assert.NotContains(t, h.output.String(), "\x1b[3J", "scrollback is never cleared")
	require.NoError(t, emulator.Close())
}

// frame replays everything written so far and returns the terminal's own screen.
func (h *ptyHarness) frame(width, height int) string {
	h.t.Helper()

	emulator := newScreenReplay(width, height)

	_, err := emulator.Write([]byte(h.output.String()))
	require.NoError(h.t, err)

	frame := emulator.String()
	require.NoError(h.t, emulator.Close())

	return frame
}
