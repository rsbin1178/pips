package coding

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// payloadState builds a live State whose transcript carries one large tool
// argument and one large image, which are the payloads State.Clone copies.
func payloadState(tb testing.TB, payloadBytes int) State {
	tb.Helper()

	args := make(ai.JSON, payloadBytes)
	args[0], args[len(args)-1] = '{', '}'
	media := bytes.Repeat([]byte{7}, payloadBytes/4)

	state := fixtureState(tb, ai.Messages{
		ai.Assistant(ai.ToolCallPart{ID: "call-big", Name: "shell", Args: args}),
		ai.User(ai.ImagePart{Source: ai.MediaSource{Data: media, MIMEType: "image/png"}}),
		ai.ToolMessage{Parts: []ai.ToolResultPart{{
			ToolCallID: "call-big", Name: "shell",
			Content: []ai.Part{
				ai.StructuredContentPart{Data: ai.JSON(`{"items":[1,2,3]}`)},
				ai.EmbeddedResourcePart{URI: "file:///payload", MIMEType: "application/octet-stream", Blob: media},
			},
		}}},
	})
	state.Sequence = 1

	return state
}

func TestStateCloneReadOnlySharesPayloadAndKeepsTransitionsIsolated(t *testing.T) {
	t.Parallel()

	state := payloadState(t, 1<<20)
	started, err := Reduce(state, Event{
		Schema: EventSchema, Sequence: state.Sequence + 1, Time: eventTestTime,
		SessionID: state.SessionID, InteractionID: state.Interaction.ID, RunID: "bench-run",
		Type: EventToolStarted,
		Payload: ToolStarted{Turn: 1, Call: ToolCall{
			ID: "call-big", Name: "shell", Arguments: ai.JSON(`{"cmd":"ls"}`),
		}},
	})
	require.NoError(t, err)

	view := started.CloneReadOnly()
	deep := started.Clone()

	viewArgs := transcriptToolArgs(t, view, 0)
	deepArgs := transcriptToolArgs(t, deep, 0)
	require.NotEmpty(t, viewArgs)
	assert.Same(t, &viewArgs[0], &transcriptToolArgs(t, started, 0)[0],
		"the read-only view shares the immutable argument bytes")
	assert.NotSame(t, &deepArgs[0], &transcriptToolArgs(t, started, 0)[0],
		"Clone still owns an independent deep copy")

	viewMedia := transcriptImageData(t, view, 1)
	deepMedia := transcriptImageData(t, deep, 1)
	require.NotEmpty(t, viewMedia)
	assert.Same(t, &viewMedia[0], &transcriptImageData(t, started, 1)[0])
	assert.NotSame(t, &deepMedia[0], &transcriptImageData(t, started, 1)[0])

	viewStructured := transcriptStructuredData(t, view, 2)
	deepStructured := transcriptStructuredData(t, deep, 2)
	require.NotEmpty(t, viewStructured)
	assert.Same(t, &viewStructured[0], &transcriptStructuredData(t, started, 2)[0])
	assert.NotSame(t, &deepStructured[0], &transcriptStructuredData(t, started, 2)[0])

	viewBlob := transcriptBlob(t, view, 2)
	deepBlob := transcriptBlob(t, deep, 2)
	require.NotEmpty(t, viewBlob)
	assert.Same(t, &viewBlob[0], &transcriptBlob(t, started, 2)[0])
	assert.NotSame(t, &deepBlob[0], &transcriptBlob(t, started, 2)[0])

	// A later transition replaces the Tools element it writes and never
	// mutates the snapshot the view already handed out.
	updated, err := Reduce(started, Event{
		Schema: EventSchema, Sequence: started.Sequence + 1, Time: eventTestTime,
		SessionID: started.SessionID, InteractionID: started.Interaction.ID, RunID: "bench-run",
		Type: EventToolUpdated,
		Payload: ToolUpdated{
			Turn: 1,
			Call: ToolCall{ID: "call-big", Name: "shell", Arguments: ai.JSON(`{"cmd":"ls"}`)},
			Update: []ai.Part{ai.TextPart{
				Text: "progress",
			}},
		},
	})
	require.NoError(t, err)
	require.Len(t, updated.Tools, 2)
	require.Len(t, view.Tools, 2)
	assert.Nil(t, view.Tools[1].Update, "the retained view does not observe later progress")
	assert.Equal(t, ToolStatusRunning, view.Tools[1].Status)
	assert.Equal(t, ToolStatusRunning, started.Tools[1].Status)
	assert.Equal(t, ToolStatusCompleted, view.Tools[0].Status, "the durable result keeps its status")
}

func transcriptToolArgs(tb testing.TB, state State, index int) ai.JSON {
	tb.Helper()

	message, ok := state.Transcript[index].(ai.AssistantMessage)
	require.True(tb, ok)
	call, ok := message.Parts[0].(ai.ToolCallPart)
	require.True(tb, ok)

	return call.Args
}

func transcriptStructuredData(tb testing.TB, state State, index int) ai.JSON {
	tb.Helper()

	content := transcriptToolContent(tb, state, index)
	structured, ok := content[0].(ai.StructuredContentPart)
	require.True(tb, ok)

	return structured.Data
}

func transcriptBlob(tb testing.TB, state State, index int) []byte {
	tb.Helper()

	content := transcriptToolContent(tb, state, index)
	resource, ok := content[1].(ai.EmbeddedResourcePart)
	require.True(tb, ok)

	return resource.Blob
}

func transcriptToolContent(tb testing.TB, state State, index int) []ai.Part {
	tb.Helper()

	message, ok := state.Transcript[index].(ai.ToolMessage)
	require.True(tb, ok)
	require.Len(tb, message.Parts, 1)

	return message.Parts[0].Content
}

func transcriptImageData(tb testing.TB, state State, index int) []byte {
	tb.Helper()

	message, ok := state.Transcript[index].(ai.UserMessage)
	require.True(tb, ok)
	image, ok := message.Parts[0].(ai.ImagePart)
	require.True(tb, ok)

	return image.Source.Data
}

var cloneBenchmarkSink State

func BenchmarkStateClonePayload(b *testing.B) {
	state := payloadState(b, 25<<20)

	b.Run("deep", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			cloneBenchmarkSink = state.Clone()
			runtime.KeepAlive(cloneBenchmarkSink)
		}
	})

	b.Run("read_only", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			cloneBenchmarkSink = state.CloneReadOnly()
			runtime.KeepAlive(cloneBenchmarkSink)
		}
	})
}
