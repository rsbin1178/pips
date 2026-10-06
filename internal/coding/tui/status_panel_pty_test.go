//go:build darwin || linux

//nolint:wsl_v5 // PTY setup, click replay and assertions follow physical order.
package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/require"
)

// TestPTYStatusPanelTabClick is the end-to-end proof of panel clicks: a real
// terminal reports a press on the painted tab, and the page switches. The click is
// resolved against the panel frame, so the transcript keeps its own gesture.
func TestPTYStatusPanelTabClick(t *testing.T) {
	const testName = "TestPTYStatusPanelTabClick"
	if os.Getenv("PIPS_TUI_PANELCLICK_HELPER") == "1" {
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
			MouseReporting: true,
			ExitOutput:     config.ExitOutputResumeHint,
			Bootstrap: func(context.Context, bool) (Controller, error) {
				// The Stats page reduces the Session store, so the helper needs a
				// controller that answers that read.
				return newOverlayController(readyState()), nil
			},
		})
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}

	h := newPTYHarness(t, "PIPS_TUI_PANELCLICK_HELPER", testName, 24, 100)
	require.Eventually(t, func() bool {
		return h.screenMatches(100, 24, func(e *vt.Emulator) bool {
			return e.IsAltScreen()
		})
	}, 30*time.Second, 10*time.Millisecond)

	h.write("/status\r")
	require.Eventually(t, func() bool {
		return h.screenMatches(100, 24, func(e *vt.Emulator) bool {
			return strings.Contains(e.String(), "Config") && strings.Contains(e.String(), "Usage")
		})
	}, 30*time.Second, 10*time.Millisecond, "the panel opens:\n%s", h.frame(100, 24))

	// Aim at the painted tab: a terminal reports the press in one-based screen
	// coordinates, and the panel resolves it against the frame it drew.
	row, column := -1, -1
	for index, line := range strings.Split(h.frame(100, 24), "\n") {
		if position := strings.Index(line, "Stats"); position >= 0 && strings.Contains(line, "Config") {
			row, column = index, position
		}
	}
	require.GreaterOrEqual(t, row, 0, "the tab bar is on screen:\n%s", h.frame(100, 24))
	require.Greater(t, column, 0)

	h.write(sgrMouse(0, column+1, row+1, false))
	require.Eventually(t, func() bool {
		return h.screenMatches(100, 24, func(e *vt.Emulator) bool {
			frame := e.String()

			return strings.Contains(frame, "Activity") ||
				strings.Contains(frame, "Reading the session store")
		})
	}, 30*time.Second, 10*time.Millisecond, "the clicked tab opens its page:\n%s", h.frame(100, 24))
}
