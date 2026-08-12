package coding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateTeamProposalUsesExactEphemeralCatalog(t *testing.T) {
	t.Parallel()

	request := validTeamProposalRequest()
	arguments := mustTeamProposalArguments(t, request)
	model := newRuntimeModel(
		runtimeToolResponse("read-1", "read", `{"path":"README.md"}`),
		runtimeToolResponse("proposal-1", teamProposalToolName, arguments),
		runtimeTextResponse("normal prompt complete"),
	)
	runtime := openGitTestRuntime(t, model, nil)
	runtime.projectInstructions = "PROJECT-PROPOSAL-BOUNDARY"
	sessionFilesBefore, err := readDirectoryNames(runtime.repository.Dir())
	require.NoError(t, err)

	var observed atomic.Int64

	runtime.observers = newAgentObservers([]func(context.Context, agent.Event){
		func(context.Context, agent.Event) { observed.Add(1) },
	})

	proposal, err := runtime.GenerateTeamProposal(t.Context(), TeamProposalPrompt{
		Objective: request.Objective,
	})
	require.NoError(t, err)
	assert.Equal(t, request, proposal.Request)
	assertNoTeamResources(t, runtime.paths)
	assert.Zero(t, observed.Load(), "ephemeral proposal events must not enter parent observers")

	sessionFilesAfter, err := readDirectoryNames(runtime.repository.Dir())
	require.NoError(t, err)
	assert.Equal(t, sessionFilesBefore, sessionFilesAfter, "proposal must not create a child Session")

	requests := model.Requests()
	require.Len(t, requests, 2)

	for _, modelRequest := range requests {
		assert.Equal(
			t,
			[]string{"read", "ls", "glob", "grep", teamProposalToolName},
			toolNamesFromRequest(modelRequest),
		)
		assert.Contains(t, requestSystemText(modelRequest), request.Objective)
		assert.Contains(t, requestSystemText(modelRequest), "PROJECT-PROPOSAL-BOUNDARY")
	}

	proposalTool := requests[0].Tools[len(requests[0].Tools)-1]
	require.NotNil(t, proposalTool.InputSchema)
	assert.ElementsMatch(
		t,
		[]string{"objective", "workers", "tasks"},
		proposalTool.InputSchema.Required,
	)
	assert.Equal(t, false, proposalTool.InputSchema.AdditionalProperties)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("continue normally")))

	requests = model.Requests()
	require.Len(t, requests, 3)
	assert.NotContains(t, toolNamesFromRequest(requests[2]), teamProposalToolName)

	require.NoError(t, runtime.DeclineTeam(t.Context(), proposal.ID))
	assertNoTeamResources(t, runtime.paths)
}

func TestGenerateTeamProposalFailuresLeaveNoAuthorityOrResources(t *testing.T) {
	t.Parallel()

	validArguments := mustTeamProposalArguments(t, validTeamProposalRequest())
	tests := map[string][]*ai.Response{
		"missing call": {
			runtimeTextResponse("Here is a prose proposal."),
		},
		"malformed call": {
			runtimeToolResponse(
				"proposal-1",
				teamProposalToolName,
				`{"objective":"x","workers":[],"tasks":[],"approved":true}`,
			),
		},
		"duplicate call": {
			runtimeToolCallsResponse(
				ai.ToolCallPart{
					ID: "proposal-1", Name: teamProposalToolName, Args: ai.JSON(validArguments),
				},
				ai.ToolCallPart{
					ID: "proposal-2", Name: teamProposalToolName, Args: ai.JSON(validArguments),
				},
			),
		},
		"model failure": nil,
	}

	for name, responses := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			runtime := openGitTestRuntime(t, newRuntimeModel(responses...), nil)
			_, err := runtime.GenerateTeamProposal(t.Context(), TeamProposalPrompt{
				Objective: validTeamProposalRequest().Objective,
			})
			require.Error(t, err)
			assertTeamProposalInactive(t, runtime)
			assertNoTeamResources(t, runtime.paths)
		})
	}
}

func TestGenerateTeamProposalCancellationLeavesNoAuthorityOrResources(t *testing.T) {
	t.Parallel()

	model := newBlockingRuntimeModel()
	runtime := openGitTestRuntime(t, model, nil)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)

	go func() {
		_, err := runtime.GenerateTeamProposal(ctx, TeamProposalPrompt{
			Objective: validTeamProposalRequest().Objective,
		})
		result <- err
	}()

	<-model.started
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	assertTeamProposalInactive(t, runtime)
	assertNoTeamResources(t, runtime.paths)
}

func TestGenerateTeamProposalPreflightFailureOccursAfterEphemeralRun(t *testing.T) {
	t.Parallel()

	probeErr := errors.New("sandbox unavailable")
	model := newRuntimeModel(runtimeToolResponse(
		"proposal-1",
		teamProposalToolName,
		mustTeamProposalArguments(t, validTeamProposalRequest()),
	))
	runtime := openGitTestRuntime(
		t,
		model,
		func(context.Context, *execution.Executor) error { return probeErr },
	)

	_, err := runtime.GenerateTeamProposal(t.Context(), TeamProposalPrompt{
		Objective: validTeamProposalRequest().Objective,
	})
	require.ErrorIs(t, err, probeErr)
	assert.Len(t, model.Requests(), 1, "the isolated Agent must finish before preflight")
	assertTeamProposalInactive(t, runtime)
	assertNoTeamResources(t, runtime.paths)
}

func TestGenerateTeamProposalRejectsUnbornRepositoryBeforeEphemeralRun(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeToolResponse(
		"proposal-1",
		teamProposalToolName,
		mustTeamProposalArguments(t, validTeamProposalRequest()),
	))
	runtime := openGitTestRuntime(t, model, nil)
	runGitTestCommand(t, runtime, "checkout", "--orphan", "unborn-team-test")

	_, err := runtime.GenerateTeamProposal(t.Context(), TeamProposalPrompt{
		Objective: validTeamProposalRequest().Objective,
	})
	require.ErrorIs(t, err, ErrTeamRepositoryRequired)
	assert.Empty(t, model.Requests(), "repository prerequisites must fail before the Lead runs")
	assertTeamProposalInactive(t, runtime)
	assertNoTeamResources(t, runtime.paths)
}

func TestGenerateTeamProposalRejectsNonRepositoryBeforeEphemeralRun(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeToolResponse(
		"proposal-1",
		teamProposalToolName,
		mustTeamProposalArguments(t, validTeamProposalRequest()),
	))
	runtime := openTestRuntime(t, model)

	_, err := runtime.GenerateTeamProposal(t.Context(), TeamProposalPrompt{
		Objective: validTeamProposalRequest().Objective,
	})
	require.ErrorIs(t, err, ErrTeamRepositoryRequired)
	assert.Empty(t, model.Requests(), "repository prerequisites must fail before the Lead runs")
	assertTeamProposalInactive(t, runtime)
	assertNoTeamResources(t, runtime.paths)
}

func TestGenerateTeamProposalRequiresAgentModeAndIdleRuntime(t *testing.T) {
	t.Parallel()

	t.Run("Agent mode", func(t *testing.T) {
		t.Parallel()

		model := newRuntimeModel()
		runtime := openGitTestRuntime(t, model, nil)
		require.NoError(t, runtime.SetMode(t.Context(), ModePlan))

		_, err := runtime.GenerateTeamProposal(t.Context(), TeamProposalPrompt{
			Objective: validTeamProposalRequest().Objective,
		})
		require.ErrorIs(t, err, ErrTeamAdmission)
		assert.Empty(t, model.Requests())
		assertTeamProposalInactive(t, runtime)
	})

	t.Run("idle Runtime", func(t *testing.T) {
		t.Parallel()

		runtime := openGitTestRuntime(t, newRuntimeModel(), nil)
		_, operation, err := runtime.beginOperation(
			t.Context(), operationPreview, runtimeResolution{}, nil,
		)
		require.NoError(t, err)

		defer runtime.endOperation(operation)

		_, err = runtime.GenerateTeamProposal(t.Context(), TeamProposalPrompt{
			Objective: validTeamProposalRequest().Objective,
		})
		require.ErrorIs(t, err, ErrRuntimeBusy)
		assertTeamProposalInactive(t, runtime)
	})
}

func TestReviseTeamProposalAtomicallyReplacesExactProposal(t *testing.T) {
	t.Parallel()

	revisedRequest := validTeamProposalRequest()
	revisedRequest.Tasks[0].Title = "Implement the revised change"
	model := newRuntimeModel(runtimeToolResponse(
		"proposal-1",
		teamProposalToolName,
		mustTeamProposalArguments(t, revisedRequest),
	))
	runtime := openGitTestRuntime(t, model, nil)
	previous, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)

	revised, err := runtime.ReviseTeamProposal(
		t.Context(), previous.ID, "Use the revised implementation title.",
	)
	require.NoError(t, err)
	assert.NotEqual(t, previous.ID, revised.ID)
	assert.Equal(t, revisedRequest, revised.Request)
	require.ErrorIs(t, runtime.DeclineTeam(t.Context(), previous.ID), ErrTeamProposalNotFound)
	assertNoTeamResources(t, runtime.paths)

	require.NoError(t, runtime.DeclineTeam(t.Context(), revised.ID))
	assertTeamProposalInactive(t, runtime)
	assertNoTeamResources(t, runtime.paths)
}

func TestReviseTeamProposalFailuresPreservePreviousProposal(t *testing.T) {
	t.Parallel()

	t.Run("model output", func(t *testing.T) {
		t.Parallel()

		model := newRuntimeModel(runtimeTextResponse("No tool call."))
		runtime := openGitTestRuntime(t, model, nil)
		previous, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
		require.NoError(t, err)

		_, err = runtime.ReviseTeamProposal(t.Context(), previous.ID, "Try a different split.")
		require.Error(t, err)
		assertCurrentTeamProposal(t, runtime, previous)
		require.NoError(t, runtime.DeclineTeam(t.Context(), previous.ID))
		assertNoTeamResources(t, runtime.paths)
	})

	t.Run("fresh preflight", func(t *testing.T) {
		t.Parallel()

		model := newRuntimeModel(runtimeToolResponse(
			"proposal-1",
			teamProposalToolName,
			mustTeamProposalArguments(t, validTeamProposalRequest()),
		))
		runtime := openGitTestRuntime(t, model, nil)
		previous, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
		require.NoError(t, err)

		probeErr := errors.New("sandbox drift")
		runtime.opts.SandboxProbe = func(context.Context, *execution.Executor) error {
			return probeErr
		}

		_, err = runtime.ReviseTeamProposal(t.Context(), previous.ID, "Keep the shape but re-check it.")
		require.ErrorIs(t, err, probeErr)
		assertCurrentTeamProposal(t, runtime, previous)
		require.NoError(t, runtime.DeclineTeam(t.Context(), previous.ID))
		assertNoTeamResources(t, runtime.paths)
	})

	t.Run("cancellation", func(t *testing.T) {
		t.Parallel()

		model := newBlockingRuntimeModel()
		runtime := openGitTestRuntime(t, model, nil)
		previous, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)

		go func() {
			_, reviseErr := runtime.ReviseTeamProposal(ctx, previous.ID, "Try a different split.")
			result <- reviseErr
		}()

		<-model.started
		cancel()
		require.ErrorIs(t, <-result, context.Canceled)
		assertCurrentTeamProposal(t, runtime, previous)
		require.NoError(t, runtime.DeclineTeam(t.Context(), previous.ID))
		assertNoTeamResources(t, runtime.paths)
	})

	t.Run("invalid feedback", func(t *testing.T) {
		t.Parallel()

		model := newRuntimeModel()
		runtime := openGitTestRuntime(t, model, nil)
		previous, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
		require.NoError(t, err)

		_, err = runtime.ReviseTeamProposal(t.Context(), previous.ID, "\x00")
		require.ErrorIs(t, err, ErrTeamAdmission)
		assert.Empty(t, model.Requests())
		assertCurrentTeamProposal(t, runtime, previous)
		require.NoError(t, runtime.DeclineTeam(t.Context(), previous.ID))
		assertNoTeamResources(t, runtime.paths)
	})

	t.Run("stale proposal", func(t *testing.T) {
		t.Parallel()

		model := newRuntimeModel()
		runtime := openGitTestRuntime(t, model, nil)
		previous, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
		require.NoError(t, err)

		runtime.admission.now = func() time.Time { return previous.ExpiresAt }

		_, err = runtime.ReviseTeamProposal(t.Context(), previous.ID, "Try a different split.")
		require.ErrorIs(t, err, ErrTeamProposalStale)
		assert.Empty(t, model.Requests())
		require.NoError(t, runtime.DeclineTeam(t.Context(), previous.ID))
		assertNoTeamResources(t, runtime.paths)
	})
}

func assertCurrentTeamProposal(t *testing.T, runtime *Runtime, expected TeamProposal) {
	t.Helper()

	record, err := runtime.currentTeamProposalRecord(expected.ID)
	require.NoError(t, err)
	assert.Equal(t, expected, record.view)
}

func assertTeamProposalInactive(t *testing.T, runtime *Runtime) {
	t.Helper()

	assert.False(t, runtime.admission.hasProposals())
	state, id := runtime.teamGuard.snapshot()
	assert.Equal(t, teamGuardInactive, state)
	assert.Empty(t, id)
}

func mustTeamProposalArguments(t *testing.T, request TeamProposalRequest) string {
	t.Helper()

	value, err := json.Marshal(request)
	require.NoError(t, err)

	return string(value)
}

func runtimeToolCallsResponse(calls ...ai.ToolCallPart) *ai.Response {
	parts := make([]ai.AssistantPart, len(calls))
	for index, call := range calls {
		parts[index] = call
	}

	return &ai.Response{
		Provider: ai.ProviderOpenAI, Model: "runtime-test",
		Message: ai.Assistant(parts...), FinishReason: ai.FinishToolCalls,
		Usage: ai.Usage{InputTokens: 10, OutputTokens: 2},
	}
}

func readDirectoryNames(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}

	return names, nil
}
