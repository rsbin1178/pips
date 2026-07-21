package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlainPresenterUsesOnlySafeSummaries(t *testing.T) {
	t.Parallel()

	const secret = "private-arguments"

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	presenter := newExecPresenter(outputPlain, stdout, stderr, false)

	require.NoError(t, presenter.Opened(coding.State{SessionID: "session-1"}))
	require.NoError(t, presenter.Event(testCLIEvent(coding.EventToolStarted, coding.ToolStarted{
		Turn: 1,
		Call: coding.ToolCall{ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"secret":"` + secret + `"}`)},
	})))
	require.NoError(t, presenter.Event(testCLIEvent(coding.EventToolCompleted, coding.ToolCompleted{
		Turn:   1,
		Call:   coding.ToolCall{ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"secret":"` + secret + `"}`)},
		Result: ai.ToolResultText("call-1", "shell", secret),
	})))

	state := coding.State{Transcript: []ai.Message{
		ai.AssistantText("old answer"),
		ai.UserText("prompt"),
		ai.Assistant(ai.Text("new "), ai.ReasoningPart{Text: secret}, ai.Text("answer")),
	}}
	require.NoError(t, presenter.Final(state, 1))

	assert.Equal(t, "new answer\n", stdout.String())
	assert.Equal(t, "session session-1 opened\ntool shell started\ntool shell completed\n", stderr.String())
	assert.NotContains(t, stderr.String(), secret)
}

func TestJSONLPresenterProjectsSafeEvents(t *testing.T) {
	t.Parallel()

	const secret = "do-not-export"

	stdout := new(bytes.Buffer)
	presenter := newExecPresenter(outputJSONL, stdout, new(bytes.Buffer), false)
	event := testCLIEvent(coding.EventToolStarted, coding.ToolStarted{
		Turn: 1,
		Call: coding.ToolCall{ID: "call-1", Name: "shell", Arguments: ai.JSON(`{"value":"` + secret + `"}`)},
	})

	require.NoError(t, presenter.Opened(coding.State{SessionID: "session-1"}))
	require.NoError(t, presenter.Event(event))
	require.NoError(t, presenter.Final(coding.State{}, 0))

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 1)
	assert.JSONEq(
		t,
		`{"schema":"pips.coding.event/v1alpha1","sequence":8,"time":"1970-01-01T00:00:01Z","session_id":"session-1","interaction_id":"interaction-1","run_id":"run-1","type":"tool.started","payload":{"turn":1,"call":{"id":"call-1","name":"shell"}}}`,
		lines[0],
	)
	assert.NotContains(t, lines[0], secret)
	decoded, err := coding.UnmarshalEvent([]byte(lines[0]))
	require.NoError(t, err)
	assert.Equal(t, event.Sequence, decoded.Sequence)
	started, ok := decoded.Payload.(coding.ToolStarted)
	require.True(t, ok)
	assert.Empty(t, started.Call.Arguments)

	var generic map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &generic))
	assert.Equal(t, coding.EventSchema, generic["schema"])
}

func TestPresenterPropagatesWriterErrors(t *testing.T) {
	t.Parallel()

	want := errors.New("writer stopped")
	presenter := newExecPresenter(outputJSONL, errorWriter{err: want}, new(bytes.Buffer), false)
	err := presenter.Event(testCLIEvent(coding.EventStatusChanged, coding.StatusChanged{Phase: coding.PhaseRunning}))
	require.ErrorIs(t, err, want)

	plain := newExecPresenter(outputPlain, errorWriter{err: want}, new(bytes.Buffer), false)
	err = plain.Final(coding.State{Transcript: []ai.Message{ai.AssistantText("answer")}}, 0)
	require.ErrorIs(t, err, want)

	short := newExecPresenter(outputPlain, shortWriter{}, new(bytes.Buffer), false)
	err = short.Final(coding.State{Transcript: []ai.Message{ai.AssistantText("answer")}}, 0)
	require.ErrorIs(t, err, io.ErrShortWrite)
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(value []byte) (int, error) { return len(value) / 2, nil }

func testCLIEvent(eventType coding.EventType, payload coding.EventPayload) coding.Event {
	event := coding.Event{
		Schema:        coding.EventSchema,
		Sequence:      8,
		Time:          time.Unix(1, 0).UTC(),
		SessionID:     "session-1",
		InteractionID: "interaction-1",
		RunID:         "run-1",
		Type:          eventType,
		Payload:       payload,
	}
	if eventType == coding.EventStatusChanged || eventType == coding.EventIntegrationDiagnostic || eventType == coding.EventError {
		event.InteractionID = ""
		event.RunID = ""
	}

	if eventType == coding.EventWorkspaceChanged || eventType == coding.EventApprovalRequired || eventType == coding.EventApprovalUnknown {
		event.RunID = ""
	}

	return event
}
