//go:build darwin || linux

//nolint:wsl_v5 // PTY launch, frame reads and teardown follow physical order.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPTYLaunchShowsTheStatusLineAndTheReservedBand is the end-to-end launch
// check: the real TUI entry runs under a PTY and its frame is read back, so the
// three surfaces this work touched are observed together on the path a user
// takes — the status line naming the model and its effective reasoning level, the
// reserved band under the transcript, and a non-empty transcript between them.
//
// It runs twice, because a frame that renders once can still be a frame that only
// renders once.
func TestPTYLaunchShowsTheStatusLineAndTheReservedBand(t *testing.T) {
	if os.Getenv("PIPS_TUI_LAUNCH_PTY_HELPER") == "1" {
		runLaunchHelper(t)

		return
	}
	t.Parallel()

	for round := range 2 {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			t.Parallel()

			harness := newPTYHarness(t, "PIPS_TUI_LAUNCH_PTY_HELPER",
				"TestPTYLaunchShowsTheStatusLineAndTheReservedBand", 24, 80)

			require.Eventually(t, func() bool {
				frame := harness.frame(80, 24)

				return containsAll(frame, launchTailMarker, "Test Model (high)")
			}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

			frame := harness.frame(80, 24)
			assert.Contains(t, frame, launchTailMarker, "the transcript is on screen")
			assert.Contains(t, frame, "Test Model (high)",
				"the status line names the model and the level it will send")
			t.Logf("ready frame:\n%s", frame)

			// The band is the row under the transcript: it is blank until the
			// reader is off the bottom, and the arrow then appears in it without
			// the status line repeating the hint.
			assert.NotContains(t, frame, bandScrollIcon)

			harness.write(sgrMouse(64, 10, 10, false)) // one wheel notch up
			require.Eventually(t, func() bool {
				return containsAll(harness.frame(80, 24), bandScrollIcon)
			}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

			scrolled := harness.frame(80, 24)
			assert.Contains(t, scrolled, bandScrollIcon)
			assert.NotContains(t, scrolled, "End for latest", "the status line does not repeat the band's hint")
			t.Logf("scrolled frame:\n%s", scrolled)

			harness.write("\x0bquit\r")
			result := harness.waitForExit()
			require.NotNil(t, result)
			require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
		})
	}
}

// runLaunchHelper is the child side: it starts the real TUI over the PTY with a
// model that declares a reasoning level, so the status line has one to name.
func runLaunchHelper(t *testing.T) {
	t.Helper()

	high := config.ReasoningLevel("high")
	controller, ok := reasoningController(t, reasoningModelConfig(), &high).(stubController)
	require.True(t, ok)

	state := readyState()
	state.Transcript = launchTranscript()
	controller.state = state

	err := Run(context.Background(), Options{
		Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
		Workspace: t.TempDir(), Trusted: true, NoColor: true,
		PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
		MouseReporting: true,
		ExitOutput:     config.ExitOutputResumeHint,
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

// launchTailMarker is the last message of the launch transcript, so the check does
// not depend on where the viewport happens to sit in a longer conversation.
const launchTailMarker = "LAUNCH-REPLY-39"

// launchTranscript is long enough that the frame cannot show all of it, which is
// what gives the reserved band an arrow to draw.
func launchTranscript() []ai.Message {
	messages := []ai.Message{ai.UserText("LAUNCH-MARKER")}
	for index := range 40 {
		messages = append(messages, ai.AssistantText(fmt.Sprintf("LAUNCH-REPLY-%02d", index)))
	}

	return messages
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}

	return true
}
