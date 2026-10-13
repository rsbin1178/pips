//go:build darwin || linux

//nolint:wsl_v5 // PTY launch and byte scans follow physical order.
package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const caretPTYEnv = "PIPS_TUI_CURSOR_STYLE_PTY_HELPER"

const (
	// caretBlinkSequence is DECSCUSR 5, a blinking bar: the style the composer asks
	// the terminal for. caretSteadySequence is 6, the same bar without the blink,
	// which is the blink flag's zero value.
	caretBlinkSequence  = "\x1b[5 q"
	caretSteadySequence = "\x1b[6 q"
)

// TestPTYComposerAsksTheTerminalForABlinkingCaret reads the bytes a real terminal
// receives. A caret that blinks is the terminal's own behaviour for the cursor
// style the app requests, and the input surfaces build that style from scratch, so
// the request is only visible in the stream.
func TestPTYComposerAsksTheTerminalForABlinkingCaret(t *testing.T) {
	if os.Getenv(caretPTYEnv) == "1" {
		runRouteClickHelper(t, func(*overlayController) {})

		return
	}
	t.Parallel()

	const cols, rows = 100, 30

	harness := newPTYHarness(t, caretPTYEnv, "TestPTYComposerAsksTheTerminalForABlinkingCaret", rows, cols)
	require.Eventually(t, func() bool {
		return strings.Contains(harness.output.String(), caretBlinkSequence)
	}, 30*time.Second, 10*time.Millisecond,
		"the composer asks for a blinking caret: %q", harness.output.String())
	assert.NotContains(t, harness.output.String(), caretSteadySequence,
		"a steady bar is the zero value of the blink flag, not a choice")
}
