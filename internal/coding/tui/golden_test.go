//nolint:wsl_v5 // Golden fixture setup is intentionally linear.
package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestViewGoldenDigests(t *testing.T) {
	t.Parallel()

	want := readViewDigests(t)
	tests := []struct {
		name    string
		width   int
		height  int
		theme   colorTheme
		noColor bool
		state   coding.State
	}{
		{
			name: "idle-dark-120x36", width: 120, height: 36, theme: themeDark,
			state: visualIdleState(),
		},
		{
			name: "running-light-80x24", width: 80, height: 24, theme: themeLight,
			state: visualRunningState(),
		},
		{
			name: "error-no-color-40x10", width: 40, height: 10,
			noColor: true, state: visualErrorState(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			controller := stubController{state: test.state}
			model := readyModelWithController(t, controller, test.noColor)
			model.theme = test.theme
			model.composer.SetStyles(composerStyles(test.theme, test.noColor))
			model.Update(tea.WindowSizeMsg{Width: test.width, Height: test.height})
			model.resetScrollback()
			stable := model.takeStableTimeline()
			model.rerenderTranscript(true)
			content := stable + "\n--- managed tail ---\n" + model.View().Content
			digest := sha256.Sum256([]byte(content))
			assert.Equal(t, want[test.name], hex.EncodeToString(digest[:]))
		})
	}
}

func visualIdleState() coding.State {
	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("Explain the event bridge and check its cancellation behavior."),
		ai.AssistantText("## Event bridge\n\nThe bridge is **bounded** and cancellation-aware."),
	}
	state.Tools = []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "read",
			Arguments: ai.JSON(`{"path":"internal/coding/tui/bridge.go"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: codingToolResultFor(
			"call-1", "read", "package tui",
		),
	}}
	state.Changes = &coding.WorkspaceChanged{
		Entries: []coding.WorkspaceChange{{Path: "internal/coding/tui/bridge.go"}},
	}

	return state
}

func visualRunningState() coding.State {
	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Transcript = []ai.Message{ai.UserText("Run the focused tests.")}
	state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta,
		Text: "I am checking the changed packages and will summarize the result.",
	}}
	state.Tools = []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "shell",
			Arguments: ai.JSON(`{"command":"go test ./internal/coding/..."}`),
		},
		Status: coding.ToolStatusRunning,
	}}

	return state
}

func visualErrorState() coding.State {
	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("Open the missing file.")}
	state.LastError = &coding.RuntimeError{
		Code: "tool_failed", Message: "the requested file does not exist",
	}

	return state
}

func readViewDigests(t *testing.T) map[string]string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "tui.sha256.golden"))
	require.NoError(t, err)
	values := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		name, digest, ok := strings.Cut(line, " ")
		if ok {
			values[name] = digest
		}
	}

	return values
}
