package coding

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// byteState is a live fixture with a small byte window, so a test can force the
// byte trim with a handful of messages.
func byteState(tb testing.TB, byteLimit int) State {
	tb.Helper()

	state := fixtureState(tb, nil)
	state.transcriptByteLimit = byteLimit
	state.toolsByteLimit = byteLimit

	return state
}

func bigText(size int) string {
	return strings.Repeat("x", size)
}

func commitText(tb testing.TB, state State, size int) State {
	tb.Helper()

	next, err := Reduce(state, boundEvent(state, EventMessageCommitted, "bench-run", MessageCommitted{
		Message: ai.UserText(bigText(size)),
	}))
	require.NoError(tb, err)

	return next
}

// TestTranscriptByteWindowTrimsOversizedMessages pins the byte bound: a window
// of a few very large messages is trimmed even though the count cap is far away.
func TestTranscriptByteWindowTrimsOversizedMessages(t *testing.T) {
	t.Parallel()

	state := byteState(t, 1<<20)
	for range 16 {
		state = commitText(t, state, 256<<10)
	}

	assert.LessOrEqual(t, state.transcriptBytes, state.transcriptByteWindow(),
		"the byte window is enforced")
	assert.Positive(t, state.TranscriptBase(), "the trim advanced the durable base")
	require.NotEmpty(t, state.Transcript)
	newest, ok := state.Transcript[len(state.Transcript)-1].(ai.UserMessage)
	require.True(t, ok)
	assert.Len(t, newest.Parts, 1,
		"the newest message is retained")
}

// TestTranscriptByteWindowKeepsOneHugeMessage pins the pathological case: a
// message larger than the whole window is kept as the only message instead of
// being dropped and leaving an empty conversation.
func TestTranscriptByteWindowKeepsOneHugeMessage(t *testing.T) {
	t.Parallel()

	state := byteState(t, 256<<10)
	state = commitText(t, state, 64<<10)
	state = commitText(t, state, 512<<10)

	require.Len(t, state.Transcript, 1)
	newest, ok := state.Transcript[0].(ai.UserMessage)
	require.True(t, ok)
	require.Len(t, newest.Parts, 1)
	text, ok := newest.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Len(t, text.Text, 512<<10)
}

// TestTranscriptByteWindowCountsMedia pins that image bytes, not just text,
// count against the window.
func TestTranscriptByteWindowCountsMedia(t *testing.T) {
	t.Parallel()

	state := byteState(t, 1<<20)

	for index := range 8 {
		media := make([]byte, 256<<10)
		next, err := Reduce(state, boundEvent(state, EventMessageCommitted, "bench-run", MessageCommitted{
			Message: ai.User(ai.ImagePart{Source: ai.MediaSource{Data: media, MIMEType: "image/png"}}),
		}))
		require.NoError(t, err)

		state = next
		assert.LessOrEqual(t, state.transcriptBytes, state.transcriptByteWindow(), "message %d", index)
	}
}

// TestTranscriptByteWindowDropsTheToolProjectionWithItsMessages pins pairing:
// the ToolState is released with the call and result messages, and the durable
// base moves so a frontend can rebase.
func TestTranscriptByteWindowDropsTheToolProjectionWithItsMessages(t *testing.T) {
	t.Parallel()

	state := byteState(t, 1<<20)
	arguments := ai.JSON(`{"path":"big.txt"}`)
	call := toolSharingCall("call-bytes", arguments)

	assistant, err := Reduce(state, boundEvent(state, EventMessageCommitted, "bench-run", MessageCommitted{
		Message: ai.Assistant(ai.ToolCallPart{ID: call.ID, Name: call.Name, Args: arguments}),
	}))
	require.NoError(t, err)
	started, err := Reduce(assistant, boundEvent(assistant, EventToolStarted, "bench-run", ToolStarted{
		Turn: 1, Call: call,
	}))
	require.NoError(t, err)

	result := structuredToolResult(call.ID, ai.JSON(`{"data":"`+bigText(256<<10)+`"}`))
	completed, err := Reduce(started, boundEvent(started, EventToolCompleted, "bench-run", ToolCompleted{
		Turn: 1, Call: call, Result: result,
	}))
	require.NoError(t, err)
	committed, err := Reduce(completed, boundEvent(completed, EventMessageCommitted, "bench-run", MessageCommitted{
		Message: result,
	}))
	require.NoError(t, err)
	require.Len(t, committed.Tools, 1)

	for range 16 {
		committed = commitText(t, committed, 256<<10)
	}

	assert.Empty(t, committed.Tools, "the released tool projection is gone")
	assert.Positive(t, committed.ToolsBase(), "the tool base advanced with it")
	assert.Positive(t, committed.TranscriptBase())
}

// TestToolsByteWindowKeepsRunningTools pins the other side: a completed
// projection is released when the Tools window is exceeded, but a running Tool
// keeps its live status.
func TestToolsByteWindowKeepsRunningTools(t *testing.T) {
	t.Parallel()

	state := byteState(t, 1<<20)

	for index := range 8 {
		call := toolSharingCall(fmt.Sprintf("call-tool-%d", index), ai.JSON(`{"path":"x"}`))

		var err error

		state, err = Reduce(state, boundEvent(state, EventToolStarted, "bench-run", ToolStarted{
			Turn: 1, Call: call,
		}))
		require.NoError(t, err)
		state, err = Reduce(state, boundEvent(state, EventToolCompleted, "bench-run", ToolCompleted{
			Turn: 1, Call: call,
			Result: structuredToolResult(call.ID, ai.JSON(`{"data":"`+bigText(256<<10)+`"}`)),
		}))
		require.NoError(t, err)
	}

	running := toolSharingCall("call-running", ai.JSON(`{"path":"live"}`))

	var err error

	state, err = Reduce(state, boundEvent(state, EventToolStarted, "bench-run", ToolStarted{
		Turn: 1, Call: running,
	}))
	require.NoError(t, err)

	assert.LessOrEqual(t, state.toolsBytes, state.toolsByteWindow())
	require.NotEmpty(t, state.Tools)
	assert.Equal(t, running.ID, state.Tools[len(state.Tools)-1].Call.ID,
		"the running Tool is retained")

	for _, tool := range state.Tools {
		if tool.Call.ID == running.ID {
			continue
		}

		assert.Equal(t, ToolStatusCompleted, tool.Status)
	}
}

// TestTranscriptByteWindowAndCountCapAgree pins the dual bound: the count cap
// still trims a window that the byte cap would allow, and the byte cap trims one
// the count would allow.
func TestTranscriptByteWindowAndCountCapAgree(t *testing.T) {
	t.Parallel()

	countBound := byteState(t, 64<<20)

	countBound.transcriptLimit = 4
	for range 12 {
		countBound = commitText(t, countBound, 64<<10)
	}

	assert.Len(t, countBound.Transcript, 4, "the count cap binds first")

	byteBound := byteState(t, 1<<20)

	byteBound.transcriptLimit = 4096
	for range 12 {
		byteBound = commitText(t, byteBound, 256<<10)
	}

	assert.Less(t, len(byteBound.Transcript), 12, "the byte cap binds first")
	assert.LessOrEqual(t, byteBound.transcriptBytes, byteBound.transcriptByteWindow())
}

// TestTranscriptByteWindowSharesTheCommittedConfigDefault pins the two packages
// to one default so the bound cannot drift.
func TestTranscriptByteWindowSharesTheCommittedConfigDefault(t *testing.T) {
	t.Parallel()

	assert.Equal(t, config.DefaultTranscriptBytes, retainedTranscriptBytes)
	assert.Equal(t, config.MaxTranscriptBytes, maxTranscriptBytes)
	assert.Equal(t, config.MinTranscriptBytes, 1<<20)
}

// TestTranscriptByteWindowBindsAtTheRealCountCap pins the dual bound where both
// caps are real: at the protocol's 4096-message window, 4096 large messages trim
// by content long before the count cap, every trim lands on the byte low water,
// and the newest message is retained whole.
func TestTranscriptByteWindowBindsAtTheRealCountCap(t *testing.T) {
	t.Parallel()

	const (
		messages = 4096
		body     = 8 << 10
		window   = 1 << 20
	)

	state := byteState(t, window)
	require.Equal(t, retainedTranscriptItems, state.transcriptWindowLimit(),
		"the count window is the protocol cap")

	lowWater := window - window/4
	previousBase := state.TranscriptBase()

	for index := range messages {
		state = commitText(t, state, body)
		require.LessOrEqual(t, state.transcriptBytes, state.transcriptByteWindow(),
			"message %d left the byte window unenforced", index)

		if state.TranscriptBase() == previousBase {
			continue
		}

		assert.LessOrEqual(t, state.transcriptBytes, lowWater,
			"a trim drops to the byte low water, not to the cap")
		previousBase = state.TranscriptBase()
	}

	assert.Positive(t, state.TranscriptBase(), "the durable base advanced")
	assert.Less(t, len(state.Transcript), messages/4, "the byte cap binds long before the count cap")
	assert.Equal(t, messages, state.TranscriptBase()+len(state.Transcript),
		"every committed message is either retained or counted into the base")

	newest, ok := state.Transcript[len(state.Transcript)-1].(ai.UserMessage)
	require.True(t, ok)
	require.Len(t, newest.Parts, 1)
	text, ok := newest.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Len(t, text.Text, body,
		"the newest message is retained whole")
}

// TestTranscriptCountCapTrimsToItsLowWater pins the count half of the dual bound
// at the real cap: one message past the window drops to 75% of it and moves the
// durable base by exactly what it dropped.
func TestTranscriptCountCapTrimsToItsLowWater(t *testing.T) {
	t.Parallel()

	state := byteState(t, maxTranscriptBytes)
	state.transcriptLimit = retainedTranscriptItems

	for range retainedTranscriptItems + 1 {
		state = commitText(t, state, 1)
	}

	lowWater := retainedTranscriptItems - retainedTranscriptItems/4
	assert.Len(t, state.Transcript, lowWater, "the count trim lands on the low water")
	assert.Equal(t, retainedTranscriptItems+1-lowWater, state.TranscriptBase(),
		"the base advances by exactly the dropped count")
	assert.LessOrEqual(t, state.transcriptBytes, state.transcriptByteWindow())
}

// TestToolsByteWindowReleasesTheProjectionNotTheTranscript pins the split behind
// the Tools window: releasing a completed ToolState drops only the live
// projection, while the call and result messages that describe it stay in the
// transcript, so a frontend can still rebuild the card and its detail page.
//
// The window counts the result bytes only until the result message commits (the
// transcript then owns them), so the release happens for tools whose results are
// still in flight — which is the parallel-tool-call shape: several tools complete
// before their results commit.
func TestToolsByteWindowReleasesTheProjectionNotTheTranscript(t *testing.T) {
	t.Parallel()

	const (
		tools      = 5
		resultBody = 256 << 10
	)

	state := byteState(t, 8<<20)
	state.toolsByteLimit = 1 << 20

	calls := make([]ToolCall, 0, tools)
	for index := range tools {
		call := toolSharingCall(fmt.Sprintf("call-window-%d", index), ai.JSON(`{"path":"x"}`))
		calls = append(calls, call)

		var err error

		state, err = Reduce(state, boundEvent(state, EventMessageCommitted, "bench-run", MessageCommitted{
			Message: ai.Assistant(ai.ToolCallPart{ID: call.ID, Name: call.Name, Args: call.Arguments}),
		}))
		require.NoError(t, err)
		state, err = Reduce(state, boundEvent(state, EventToolStarted, "bench-run", ToolStarted{
			Turn: 1, Call: call,
		}))
		require.NoError(t, err)
		state, err = Reduce(state, boundEvent(state, EventToolCompleted, "bench-run", ToolCompleted{
			Turn: 1, Call: call,
			Result: structuredToolResult(call.ID, ai.JSON(`{"data":"`+bigText(resultBody)+`"}`)),
		}))
		require.NoError(t, err)
	}

	require.Less(t, len(state.Tools), tools, "the Tools window released the oldest projections")
	require.Positive(t, state.ToolsBase())
	assert.Zero(t, state.TranscriptBase(), "the transcript window did not bind")
	require.Len(t, state.Transcript, tools, "every call message is still retained")

	live := make(map[string]struct{}, len(state.Tools))
	for _, tool := range state.Tools {
		live[tool.Call.ID] = struct{}{}
	}

	released := calls[:state.ToolsBase()]
	durable := toolCallIDsInMessages(state.Transcript)

	for _, call := range released {
		_, projected := live[call.ID]
		assert.False(t, projected, "the released projection is gone from Tools")

		_, retained := durable[call.ID]
		assert.True(t, retained, "the released call is still in the transcript")
	}

	// Committing the released tools' results leaves the transcript as the only
	// owner of both halves of the call, which is what makes the card rebuildable.
	for _, call := range released {
		next, err := Reduce(state, boundEvent(state, EventMessageCommitted, "bench-run", MessageCommitted{
			Message: structuredToolResult(call.ID, ai.JSON(`{"data":"`+bigText(resultBody)+`"}`)),
		}))
		require.NoError(t, err)

		state = next
	}

	durable = toolCallIDsInMessages(state.Transcript)
	for _, call := range released {
		_, retained := durable[call.ID]
		assert.True(t, retained, "call %s keeps both halves in the transcript", call.ID)
	}

	assert.Zero(t, state.TranscriptBase())
}
