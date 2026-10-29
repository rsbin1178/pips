//nolint:wsl_v5 // One tool transition and its retained projection stay adjacent.
package coding

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolEvent(state State, eventType EventType, payload EventPayload) Event {
	return Event{
		Schema: EventSchema, Sequence: state.Sequence + 1, Time: eventTestTime,
		SessionID: state.SessionID, InteractionID: state.Interaction.ID, RunID: "bench-run",
		Type: eventType, Payload: payload,
	}
}

func startBenchTool(t *testing.T, state State) State {
	t.Helper()

	next, err := Reduce(state, toolEvent(state, EventToolStarted, ToolStarted{
		Turn: 1,
		Call: ToolCall{ID: "call-progress", Name: "shell", Arguments: ai.JSON(`{"cmd":"run"}`)},
	}))
	require.NoError(t, err)

	return next
}

func TestToolCompletedClearsProgressAndKeepsResult(t *testing.T) {
	t.Parallel()

	state := startBenchTool(t, fixtureState(t, nil))
	state, err := Reduce(state, toolEvent(state, EventToolUpdated, ToolUpdated{
		Turn:   1,
		Call:   ToolCall{ID: "call-progress", Name: "shell", Arguments: ai.JSON(`{"cmd":"run"}`)},
		Update: []ai.Part{ai.TextPart{Text: "live progress"}},
	}))
	require.NoError(t, err)
	require.Len(t, state.Tools, 1)
	require.NotEmpty(t, state.Tools[0].Update)

	completed, err := Reduce(state, toolEvent(state, EventToolCompleted, ToolCompleted{
		Turn: 1,
		Call: ToolCall{ID: "call-progress", Name: "shell", Arguments: ai.JSON(`{"cmd":"run"}`)},
		Result: ai.ToolMessage{Parts: []ai.ToolResultPart{{
			ToolCallID: "call-progress", Name: "shell",
			Content: []ai.Part{ai.TextPart{Text: "durable result"}},
		}}},
	}))
	require.NoError(t, err)
	require.Len(t, completed.Tools, 1)
	assert.Nil(t, completed.Tools[0].Update, "a completed Tool keeps no live progress body")
	assert.Equal(t, ToolStatusCompleted, completed.Tools[0].Status)

	result := completed.Tools[0].Result
	require.Len(t, result.Parts, 1)
	content := result.Parts[0].Content
	require.Len(t, content, 1)
	text, ok := content[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "durable result", text.Text)
}

func TestToolUpdateIsBounded(t *testing.T) {
	t.Parallel()

	state := startBenchTool(t, fixtureState(t, nil))
	body := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB
	state, err := Reduce(state, toolEvent(state, EventToolUpdated, ToolUpdated{
		Turn:   1,
		Call:   ToolCall{ID: "call-progress", Name: "shell", Arguments: ai.JSON(`{"cmd":"run"}`)},
		Update: []ai.Part{ai.TextPart{Text: body}},
	}))
	require.NoError(t, err)
	require.Len(t, state.Tools, 1)
	require.Len(t, state.Tools[0].Update, 1)
	text, ok := state.Tools[0].Update[0].(ai.TextPart)
	require.True(t, ok)
	assert.LessOrEqual(t, len(text.Text), maxToolUpdateBytes)
	assert.True(t, strings.HasPrefix(body, text.Text))

	// A later full snapshot replaces the body instead of accumulating it.
	state, err = Reduce(state, toolEvent(state, EventToolUpdated, ToolUpdated{
		Turn:   1,
		Call:   ToolCall{ID: "call-progress", Name: "shell", Arguments: ai.JSON(`{"cmd":"run"}`)},
		Update: []ai.Part{ai.TextPart{Text: "latest"}},
	}))
	require.NoError(t, err)
	require.Len(t, state.Tools[0].Update, 1)
	text, ok = state.Tools[0].Update[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "latest", text.Text)
}

func TestToolUpdateBoundsPartCountAndUTF8(t *testing.T) {
	t.Parallel()

	state := startBenchTool(t, fixtureState(t, nil))

	parts := make([]ai.Part, maxToolUpdateParts*2)
	for index := range parts {
		parts[index] = ai.TextPart{Text: "x"}
	}

	state, err := Reduce(state, toolEvent(state, EventToolUpdated, ToolUpdated{
		Turn: 1, Call: ToolCall{ID: "call-progress", Name: "shell", Arguments: ai.JSON(`{"cmd":"run"}`)}, Update: parts,
	}))
	require.NoError(t, err)
	assert.Len(t, state.Tools[0].Update, maxToolUpdateParts)

	// Cut in the middle of a three-byte rune: the retained text must stay
	// valid UTF-8 and be a prefix of the reported body.
	body := strings.Repeat("\u4e16", maxToolUpdateBytes)
	state, err = Reduce(state, toolEvent(state, EventToolUpdated, ToolUpdated{
		Turn: 1, Call: ToolCall{ID: "call-progress", Name: "shell", Arguments: ai.JSON(`{"cmd":"run"}`)},
		Update: []ai.Part{ai.TextPart{Text: body}},
	}))
	require.NoError(t, err)
	text, ok := state.Tools[0].Update[0].(ai.TextPart)
	require.True(t, ok)
	assert.True(t, utf8.ValidString(text.Text))
	assert.True(t, strings.HasPrefix(body, text.Text))
	assert.LessOrEqual(t, len(text.Text), maxToolUpdateBytes)
}

func TestFailedRunClearsLiveProgress(t *testing.T) {
	t.Parallel()

	state := startBenchTool(t, fixtureState(t, nil))
	state, err := Reduce(state, toolEvent(state, EventToolUpdated, ToolUpdated{
		Turn:   1,
		Call:   ToolCall{ID: "call-progress", Name: "shell", Arguments: ai.JSON(`{"cmd":"run"}`)},
		Update: []ai.Part{ai.TextPart{Text: "live progress"}},
	}))
	require.NoError(t, err)

	interrupted, err := Reduce(state, toolEvent(state, EventError, RuntimeError{
		Code: "model_failed", Fatal: true,
	}))
	require.NoError(t, err)
	require.Len(t, interrupted.Tools, 1)
	assert.Nil(t, interrupted.Tools[0].Update)
	assert.Equal(t, ToolStatusCompleted, interrupted.Tools[0].Status)
}

func TestCloneReadOnlyAfterProgressClears(t *testing.T) {
	t.Parallel()

	state := startBenchTool(t, fixtureState(t, nil))
	view := state.CloneReadOnly()
	assert.Nil(t, view.Tools[0].Update)
	assert.NotNil(t, view.Tools[0].Call.Arguments)
}
