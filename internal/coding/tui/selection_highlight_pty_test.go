//go:build darwin || linux

//nolint:wsl_v5 // PTY setup, gesture replay and cell assertions follow physical order.
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
	"github.com/stretchr/testify/require"
)

// reverseAttrMask is cellbuf's ReverseAttr (1<<5). The package is only an indirect
// dependency of this module, so the test names the bit instead of adding a direct
// requirement for it.
const reverseAttrMask = 32

// TestPTYSelectionHighlightCoversStyledCells is the end-to-end proof that a drag
// paints every addressed cell, including a row that carries its own inline styling:
// a reset inside the addressed span used to cancel the reverse video, so most of a
// styled row showed no selection background at all.
//
// The repo harness pins NO_COLOR=1 for the helper process; colour is the point of
// this test, so the helper hands Bubble Tea an environment without it.
func TestPTYSelectionHighlightCoversStyledCells(t *testing.T) {
	const testName = "TestPTYSelectionHighlightCoversStyledCells"
	if os.Getenv("PIPS_TUI_SELHIGHLIGHT_HELPER") == "1" {
		state := readyState()
		state.Transcript = []ai.Message{
			ai.UserText("COPY-ROW-ALPHA"),
			ai.AssistantText("STYLED-ROW with `inline code` and **bold**"),
			ai.UserText("COPY-ROW-BETA"),
		}
		environment := make([]string, 0, len(os.Environ()))
		for _, entry := range os.Environ() {
			if strings.HasPrefix(entry, "NO_COLOR=") {
				continue
			}
			environment = append(environment, entry)
		}
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: environment,
			Workspace: t.TempDir(), Trusted: true, NoColor: false,
			PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
			MouseReporting: true,
			ExitOutput:     config.ExitOutputResumeHint,
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

	h := newPTYHarness(t, "PIPS_TUI_SELHIGHLIGHT_HELPER", testName, 24, 80)
	require.Eventually(t, func() bool {
		return h.screenMatches(80, 24, func(e *vt.Emulator) bool {
			return strings.Contains(e.String(), "STYLED-ROW")
		})
	}, 30*time.Second, 10*time.Millisecond)

	frame := strings.Split(h.frame(80, 24), "\n")
	styled := -1
	for index, line := range frame {
		if strings.Contains(line, "STYLED-ROW") {
			styled = index
		}
	}
	require.GreaterOrEqual(t, styled, 0)

	// A press alone selects one cell and paints nothing; the drag is what addresses
	// a span, so the gesture is replayed press, motion and checked there.
	h.write(sgrMouse(0, 3, styled+1, false))
	h.write(sgrMouse(32, 45, styled+1, false))

	during := false
	require.Eventually(t, func() bool {
		during = h.screenMatches(80, 24, func(e *vt.Emulator) bool {
			for column := 2; column < 40; column++ {
				cell := e.CellAt(column, styled)
				if cell == nil || cell.Style.Attrs&reverseAttrMask == 0 {
					return false
				}
			}

			return true
		})

		return during
	}, 30*time.Second, 10*time.Millisecond, "the drag paints the addressed cells")
	require.True(t, during, "every addressed cell carries the highlight")
}
