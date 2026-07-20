package team

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContinuationCrashWindowsWithLocalStores(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*testing.T) continuation.Store{
		"memory": func(t *testing.T) continuation.Store {
			t.Helper()

			store, err := continuation.NewMemoryStore()
			require.NoError(t, err)

			return store
		},
		"jsonl": func(t *testing.T) continuation.Store {
			t.Helper()

			store, err := continuation.NewJSONLStore(filepath.Join(t.TempDir(), "continuations"))
			require.NoError(t, err)

			return store
		},
	}

	for name, newStore := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			teamDir := filepath.Join(t.TempDir(), "teams")
			teamStore, err := NewJSONLStore(teamDir)
			require.NoError(t, err)
			runtime := newTestRuntime(t, teamStore)
			team := registerWorker(t, runtime, createTestTeam(t, runtime))

			team, err = runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
				Command: runtime.coordinator(team.Revision), TaskID: "task",
				Title: "Run in an independent session", AttemptLimit: 2,
			})
			require.NoError(t, err)
			team, err = runtime.engine.ClaimTask(t.Context(), team.ID, ClaimTaskRequest{
				Command: runtime.member("worker", team.Revision), TaskID: "task",
			})
			require.NoError(t, err)
			started, err := runtime.engine.StartTaskAttempt(t.Context(), team.ID, StartTaskAttemptRequest{
				Command: runtime.coordinator(team.Revision), TaskID: "task",
				AttemptID: "attempt", ContinuationID: "child-execution",
			})
			require.NoError(t, err)

			child, err := continuation.New(newStore(t))
			require.NoError(t, err)
			inspections, err := runtime.engine.InspectActiveAttempts(t.Context(), team.ID, child)
			require.NoError(t, err)
			require.Len(t, inspections, 1)
			assert.Equal(t, AttemptInspectionMissing, inspections[0].State)

			execution, err := child.Create(t.Context(), continuation.CreateRequest{
				ID: started.Dispatch.ContinuationID,
				Target: continuation.Target{
					Kind: "team_task", ID: string(started.Dispatch.AttemptID),
				},
				Worker: continuation.HandlerRef{Kind: "harness_agent", Version: "v1"},
				Controller: continuation.HandlerRef{
					Kind: "team_task_controller", Version: "v1",
				},
				Input: started.Dispatch.Payload,
			})
			require.NoError(t, err)
			inspections, err = runtime.engine.InspectActiveAttempts(t.Context(), team.ID, child)
			require.NoError(t, err)
			assert.Equal(t, AttemptInspectionNonterminal, inspections[0].State)

			execution, err = child.Cancel(
				t.Context(), execution.ID, execution.Revision, "integration cancellation",
			)
			require.NoError(t, err)
			inspections, err = runtime.engine.InspectActiveAttempts(t.Context(), team.ID, child)
			require.NoError(t, err)
			assert.Equal(t, AttemptInspectionTerminal, inspections[0].State)
			require.NotNil(t, inspections[0].Execution)
			assert.Equal(t, continuation.StatusCancelled, inspections[0].Execution.Status)

			reopenedStore, err := NewJSONLStore(teamDir)
			require.NoError(t, err)
			reopened, err := New(reopenedStore)
			require.NoError(t, err)

			finish := FinishTaskAttemptRequest{
				Command: CommandMetadata{
					ID: "reconcile-terminal", ExpectedRevision: started.Team.Revision,
					Actor: Actor{Kind: ActorKindCoordinator, ID: "restart-coordinator"},
				},
				TaskID: "task", AttemptID: "attempt", ContinuationID: "child-execution",
				Outcome: AttemptOutcomeFailed, Reason: "child cancelled",
			}
			finished, err := reopened.FinishTaskAttempt(t.Context(), team.ID, finish)
			require.NoError(t, err)
			assert.Equal(t, TaskStatusFailed, finished.Tasks[0].Status)

			finish.Command.ExpectedRevision = finished.Revision + 10
			replayed, err := reopened.FinishTaskAttempt(t.Context(), team.ID, finish)
			require.NoError(t, err)
			assert.Equal(t, finished, replayed)
		})
	}
}

type harnessTextModel struct{}

func (*harnessTextModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return &ai.Response{
		Message: ai.AssistantText("independent result"), FinishReason: ai.FinishStop,
	}, nil
}

func (*harnessTextModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(func(ai.StreamEvent, error) bool) {}
}

func (*harnessTextModel) Provider() ai.Provider { return "team-harness-test" }
func (*harnessTextModel) ModelID() string       { return "team-harness-test-1" }
func (*harnessTextModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true}
}

func TestDispatchSelectsIndependentHarnessSession(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))
	team, err := runtime.engine.CreateTask(t.Context(), team.ID, CreateTaskRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task",
		Title: "Independent work", Payload: ai.JSON(`{"input":1}`), AttemptLimit: 1,
	})
	require.NoError(t, err)
	team, err = runtime.engine.ClaimTask(t.Context(), team.ID, ClaimTaskRequest{
		Command: runtime.member("worker", team.Revision), TaskID: "task",
	})
	require.NoError(t, err)
	started, err := runtime.engine.StartTaskAttempt(t.Context(), team.ID, StartTaskAttemptRequest{
		Command: runtime.coordinator(team.Revision), TaskID: "task",
		AttemptID: "attempt", ContinuationID: "execution",
	})
	require.NoError(t, err)

	leadSession, err := harness.NewSession(harness.NewMemoryStore("session-lead"))
	require.NoError(t, err)
	workerSession, err := harness.NewSession(harness.NewMemoryStore(started.Dispatch.SessionRef))
	require.NoError(t, err)
	workerHarness, err := harness.New(&harnessTextModel{}, workerSession)
	require.NoError(t, err)
	result, err := workerHarness.Prompt(t.Context(), started.Dispatch.Title)
	require.NoError(t, err)

	assert.Equal(t, "session-worker", workerSession.Metadata().ID)
	assert.Equal(t, "independent result", result.Text())
	assert.Empty(t, leadSession.Entries())
	assert.Len(t, workerSession.Entries(), 2)
}
