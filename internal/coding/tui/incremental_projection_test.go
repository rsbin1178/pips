package tui

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIncrementalProjectionSharesStateBacking pins the inline projection's
// memory contract: the committed prefix is sliced away, not deep-cloned, so one
// inline frame or stable commit does not copy the whole conversation.
func TestIncrementalProjectionSharesStateBacking(t *testing.T) {
	t.Parallel()

	state := bulkState(12)
	state.Tools = []coding.ToolState{
		{Call: coding.ToolCall{ID: "call-1", Name: "read"}},
		{Call: coding.ToolCall{ID: "call-2", Name: "read"}},
	}
	state.Diagnostics = []coding.IntegrationDiagnostic{
		{Code: "first"},
		{Code: "second"},
	}
	state.MessageCandidates = make([]coding.CandidateIdentity, len(state.Transcript))
	state.SyntheticMessages = []int{0, 1, 2, 3}

	model := newModel(t.Context(), Options{NoColor: true})
	model.state = state
	model.scrollback = scrollbackCursor{
		messages: 2, tools: 1, diagnostics: 1,
	}

	projected := model.incrementalProjection(len(state.Transcript), len(state.Tools))

	require.Len(t, projected.Transcript, len(state.Transcript)-2)
	assert.Same(t, &state.Transcript[2], &projected.Transcript[0],
		"the first projected message shares the State backing array")
	assert.Same(t, &state.Tools[1], &projected.Tools[0],
		"the first projected Tool shares the State backing array")
	assert.Same(t, &state.Diagnostics[1], &projected.Diagnostics[0],
		"the first projected diagnostic shares the State backing array")
	assert.Len(t, projected.MessageCandidates, len(projected.Transcript))
	assert.Equal(t, []int{0, 1}, projected.SyntheticMessages,
		"synthetic indexes are rebased onto the projected transcript")

	assert.Len(t, model.state.Transcript, len(state.Transcript),
		"the projection does not mutate the live State")
	assert.IsType(t, ai.UserMessage{}, model.state.Transcript[0])
}

// TestIncrementalProjectionStableEndKeepsTheMutableTail asserts the stable
// commit projects only the delta, while the live tail projects through the end.
func TestIncrementalProjectionStableEndKeepsTheMutableTail(t *testing.T) {
	t.Parallel()

	state := bulkState(10)
	model := newModel(t.Context(), Options{NoColor: true})
	model.state = state
	model.scrollback = scrollbackCursor{messages: 4}

	stable := model.incrementalProjection(7, 0)
	assert.Len(t, stable.Transcript, 3, "the stable projection stops at its frontier")

	active := model.incrementalProjection(len(model.state.Transcript), 0)
	assert.Len(t, active.Transcript, 6, "the live tail reaches the newest message")
}
