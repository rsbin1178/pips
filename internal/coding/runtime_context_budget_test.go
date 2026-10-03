package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/compaction"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type contextOverflowModel struct {
	*runtimeModel
	failures map[int]bool
	streams  int
	partial  bool
}

func (m *contextOverflowModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		m.mu.Lock()
		m.streams++

		fail := m.failures[m.streams]
		if fail {
			m.requests = append(m.requests, request)
		}
		m.mu.Unlock()

		if fail {
			if m.partial {
				if !yield(ai.StreamEvent{Type: ai.StreamMessageStart, Provider: m.Provider(), Model: m.ModelID()}, nil) ||
					!yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "partial answer"}, nil) {
					return
				}
			}

			failure := ai.NewError(ai.ProviderOpenAI, 400, "maximum context length exceeded")
			failure.Code = "context_length_exceeded"
			yield(ai.StreamEvent{}, failure)

			return
		}

		for event, err := range m.runtimeModel.Stream(ctx, request) {
			if !yield(event, err) {
				return
			}
		}
	}
}

func TestRuntimeRecoversContextOnceBeforeAnyModelOutput(t *testing.T) {
	t.Parallel()

	model := &contextOverflowModel{
		runtimeModel: newRuntimeModel(checkpointResponse(), runtimeTextResponse("recovered")),
		failures:     map[int]bool{1: true},
	}
	runtime := openTestRuntime(t, model)
	configureRuntimeCompaction(runtime, 100000, 16000, 20000, 4096)
	runtime.config.Compaction.MinSummaryChars = 500
	appendRuntimeHistory(t, runtime, 12000, 12000, 1000, 1000)
	events, err := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText("continue after overflow")))
	require.NoError(t, err)
	assert.Equal(t, 1, countEventType(events, EventRunInterrupted))
	assert.Equal(t, 1, countEventType(events, EventCompactionCompleted))
	assert.Equal(t, 2, countEventType(events, EventRunStarted))
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
	assert.Len(t, model.Requests(), 3)

	userInputs := 0

	for _, entry := range runtime.session.Path() {
		if entry.Kind == harness.KindMessage {
			if text, ok := singleUserText(entry.Message); ok && text == "continue after overflow" {
				userInputs++
			}
		}
	}

	assert.Equal(t, 1, userInputs, "retry must not append the user's input twice")
}

func TestRuntimeDoesNotLoopContextRecovery(t *testing.T) {
	t.Parallel()

	model := &contextOverflowModel{runtimeModel: newRuntimeModel(checkpointResponse()), failures: map[int]bool{1: true, 2: true}}
	runtime := openTestRuntime(t, model)
	configureRuntimeCompaction(runtime, 100000, 16000, 20000, 4096)
	appendRuntimeHistory(t, runtime, 12000, 12000, 1000, 1000)
	events, err := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText("continue")))
	require.Error(t, err)
	assert.Equal(t, 1, countEventType(events, EventRunInterrupted))
	assert.Equal(t, 1, countEventType(events, EventCompactionCompleted))
	assert.Len(t, model.Requests(), 3)
	assert.Equal(t, InteractionFailed, runtime.Snapshot().Interaction.Outcome)
}

func TestRuntimeDoesNotReplayPartialOutputOrTools(t *testing.T) {
	t.Parallel()

	for _, partial := range []bool{true, false} {
		name := "after_tool"
		if partial {
			name = "partial_text"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			model := &contextOverflowModel{runtimeModel: newRuntimeModel(), failures: map[int]bool{1: true}, partial: partial}
			if !partial {
				model.runtimeModel = newRuntimeModel(runtimeToolResponse("ls-before-overflow", "ls", `{"path":"."}`))
				model.failures = map[int]bool{2: true}
			}

			runtime := openTestRuntime(t, model)
			configureRuntimeCompaction(runtime, 100000, 16000, 20000, 4096)
			appendRuntimeHistory(t, runtime, 1000, 1000)
			events, err := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText("inspect")))
			require.Error(t, err)
			assert.Zero(t, countEventType(events, EventRunInterrupted))
			assert.Zero(t, countEventType(events, EventCompactionCompleted))
		})
	}
}

func TestRuntimeReservesIncomingInputBeforeCompactionCommit(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(checkpointResponse(), runtimeTextResponse("done"))
	runtime := openTestRuntime(t, model)
	configureRuntimeCompaction(runtime, 100000, 16000, 20000, 4096)
	appendRuntimeHistory(t, runtime, 30000, 30000, 1000, 1000)

	incoming := "new-demand: " + strings.Repeat("word", 30000)
	events, err := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText(incoming)))
	require.NoError(t, err)
	assert.Equal(t, 1, countEventType(events, EventCompactionCompleted))

	ids := compaction.ArchiveIDs(runtime.session.Path())
	require.Len(t, ids, 1)
	reader, err := runtime.bindHistory(t.Context())
	require.NoError(t, err)
	page, err := reader.Search(t.Context(), session.ArchiveSearchRequest{ArchiveID: ids[0], Query: "new-demand"})
	require.NoError(t, err)
	assert.Empty(t, page.Matches, "incoming user input is not in the earlier archive snapshot")
	assert.Len(t, model.Requests(), 2)
}

func TestRuntimeRejectsOversizedIncomingInputWithoutAppending(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntime(t, model)
	configureRuntimeCompaction(runtime, 100000, 16000, 20000, 4096)
	before := runtime.session.Entries()
	_, err := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText(strings.Repeat("word", 100000))))
	require.ErrorIs(t, err, compaction.ErrBudget)
	assert.Equal(t, before, runtime.session.Entries())
	assert.Empty(t, model.Requests())
}

func TestRuntimeInvalidatesUsageWhenPromptPolicyChanges(t *testing.T) {
	t.Parallel()
	runtime := openTestRuntime(t, newRuntimeModel())
	_, err := runtime.session.AppendMessage(ai.UserText("task"), nil)
	require.NoError(t, err)
	runtime.recordContextRequest(&ai.Request{Messages: ai.Messages{ai.SystemText("first policy")}})
	_, err = runtime.session.AppendMessage(ai.AssistantText("response"), &ai.Usage{InputTokens: 9000, OutputTokens: 1000})
	require.NoError(t, err)
	assert.Equal(t, 10000, runtime.contextTokens())

	request := ai.Request{Messages: ai.Messages{ai.SystemText(strings.Repeat("new policy ", 100))}}
	runtime.recordContextRequest(&request)
	fixed, err := compaction.RequestOverheadTokens(request)
	require.NoError(t, err)
	visible, err := runtime.session.Context()
	require.NoError(t, err)
	assert.Equal(t, compaction.SeedTokens(visible.Messages, fixed), runtime.contextTokens())
	_, err = runtime.session.AppendMessage(ai.AssistantText("fresh"), &ai.Usage{InputTokens: 1500, OutputTokens: 200})
	require.NoError(t, err)
	assert.Equal(t, 1700, runtime.contextTokens())
}
