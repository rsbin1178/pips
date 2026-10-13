package coding

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/extension"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingToolExtension contributes one read Tool that parks until the run
// context ends, so a test can stop a turn while a call is in flight.
func blockingToolExtension() extension.Extension {
	value, err := extension.NewDefinition(extension.Descriptor{
		ID: "block-tool", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		tool := agent.NewTool("extension_block", "Block until the run ends.", func(
			ctx context.Context,
			_ struct{},
		) (string, error) {
			<-ctx.Done()

			return "", ctx.Err()
		})

		return extension.Contribution{
			Tools: []extension.Tool{{Value: tool, Risk: catalog.RiskRead}},
		}, nil
	})
	if err != nil {
		panic(err)
	}

	return value
}

func hasStateDiagnostic(state State, component, code string) bool {
	for _, diagnostic := range state.Diagnostics {
		if diagnostic.Component == component && diagnostic.Code == code {
			return true
		}
	}

	return false
}

// TestRuntimeStopMidToolBatchLeavesResumableSession covers a frontend that
// stops consuming the run while a Tool call is in flight: the TUI's Esc and
// Ctrl+C cancel the operation and drop the event bridge, so the agent message
// carrying the call is already durable when the results never arrive. The
// session must still reopen, because the whole point of the stop is to come
// back to it.
func TestRuntimeStopMidToolBatchLeavesResumableSession(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := newRuntimeModel(
		runtimeToolResponse("call-block", "extension_block", `{}`),
		runtimeTextResponse("first follow-up"),
		runtimeTextResponse("second follow-up"),
	)
	runtime := openTestRuntimeAtWithExtensions(
		t, base, SessionTarget{}, model, []extension.Extension{blockingToolExtension()},
	)

	for event, err := range runtime.Prompt(t.Context(), ai.UserText("go")) {
		require.NoError(t, err)

		if event.Type == EventToolStarted {
			break
		}
	}

	pending, err := runtime.session.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending, "the stop answers the call it left unexecuted")

	recovery, err := runtime.journal.replay()
	require.NoError(t, err)
	assert.Empty(t, recovery.PendingID, "the interaction reached its terminal record")

	// The stop does not cost the session: a new prompt runs in place, and the
	// next reopen finds the same tail.
	again := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("keep going")))
	assert.Contains(t, eventTypes(again), EventRunCompleted)

	sessionID := runtime.handle.Metadata().ID
	abruptRuntimeStop(t, runtime)

	reopened := openTestRuntimeAt(
		t, base, SessionTarget{ID: sessionID}, newRuntimeModel(runtimeTextResponse("continued")),
	)
	assert.Equal(t, PhaseIdle, reopened.Snapshot().Phase)
	assert.False(
		t, hasStateDiagnostic(reopened.Snapshot(), "runtime", "interaction_interrupted"),
		"a settled tail needs no interrupt diagnostic",
	)

	events := collectRuntimeEvents(t, reopened.Prompt(t.Context(), ai.UserText("carry on")))
	assert.Contains(t, eventTypes(events), EventRunCompleted)
	require.NoError(t, reopened.Close(t.Context()))
}

// TestRuntimeReopensSessionWithOrphanedToolCalls covers a session written
// before the harness answered a stop mid-batch: the tail holds a call with no
// result while the journal holds no pending interaction. Opening repairs the
// tail and reports it, so a durable session is never unopenable.
func TestRuntimeReopensSessionWithOrphanedToolCalls(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openTestRuntimeAt(t, base, SessionTarget{}, newRuntimeModel(runtimeTextResponse("unused")))

	_, err := first.session.AppendMessage(ai.UserText("go"), nil)
	require.NoError(t, err)
	_, err = first.session.AppendMessage(ai.Assistant(ai.ToolCallPart{
		ID: "call-orphan", Name: "shell", Args: ai.JSON(`{"command":"true"}`),
	}), nil)
	require.NoError(t, err)

	interactionID, err := first.journal.start()
	require.NoError(t, err)
	require.NoError(t, first.journal.complete(
		interactionID, InteractionCanceled, "", TokenUsage{}, 0, interactionTally{},
	))

	sessionID := first.handle.Metadata().ID
	abruptRuntimeStop(t, first)

	second := openTestRuntimeAt(
		t, base, SessionTarget{ID: sessionID}, newRuntimeModel(runtimeTextResponse("continued")),
	)

	snapshot := second.Snapshot()
	assert.True(
		t, hasStateDiagnostic(snapshot, "runtime", "interaction_interrupted"),
		"the repair is reported",
	)

	pending, err := second.session.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending, "opening settles the orphaned calls")

	var settled []ai.ToolResultPart

	for _, message := range snapshot.Transcript {
		toolMessage, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}

		settled = append(settled, toolMessage.Parts...)
	}

	require.Len(t, settled, 1, "the settled answer is part of the transcript")
	assert.Equal(t, "call-orphan", settled[0].ToolCallID)
	assert.True(t, settled[0].IsError)

	answer, ok := settled[0].Content[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, answer.Text, "was not executed")

	events := collectRuntimeEvents(t, second.Prompt(t.Context(), ai.UserText("carry on")))
	assert.Contains(t, eventTypes(events), EventRunCompleted)
	require.NoError(t, second.Close(t.Context()))
}
