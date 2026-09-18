package coding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/teamcontrol"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/rsbin1178/pips/internal/coding/teamworktree"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamProposalFailuresCreateNoDurableResources(t *testing.T) {
	t.Parallel()

	t.Run("invalid DAG", func(t *testing.T) {
		t.Parallel()

		runtime := openGitTestRuntime(t, newRuntimeModel(), nil)
		request := validTeamProposalRequest()
		request.Tasks = append(request.Tasks, TeamTaskSpec{
			ID: "task-2", Title: "Second", AssignedWorker: "Implementer",
			Dependencies: []string{"task-1"},
		})
		request.Tasks[0].Dependencies = []string{"task-2"}

		_, err := runtime.ProposeTeam(t.Context(), request)
		assert.ErrorIs(t, err, ErrTeamAdmission)
		assertNoTeamResources(t, runtime.paths)
	})

	t.Run("sandbox probe", func(t *testing.T) {
		t.Parallel()

		probeErr := errors.New("sandbox unavailable")
		runtime := openGitTestRuntime(t, newRuntimeModel(), nil)
		runtime.opts.SandboxProbe = func(context.Context, *execution.Executor) error {
			return probeErr
		}

		_, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
		assert.ErrorIs(t, err, probeErr)
		assertNoTeamResources(t, runtime.paths)
	})
}

func TestTeamProposalDeclineCreatesNoDurableResources(t *testing.T) {
	t.Parallel()

	runtime := openGitTestRuntime(t, newRuntimeModel(), nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	assert.False(t, proposal.Dirty)
	assertNoTeamResources(t, runtime.paths)

	require.NoError(t, runtime.DeclineTeam(t.Context(), proposal.ID))
	assertNoTeamResources(t, runtime.paths)
	state, id := runtime.teamGuard.snapshot()
	assert.Equal(t, teamGuardInactive, state)
	assert.Empty(t, id)
}

func TestTeamNonInteractiveConfirmationRequiresInputWithoutConsumingProposal(t *testing.T) {
	t.Parallel()

	runtime := openGitTestRuntime(t, newRuntimeModel(), nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)

	_, err = runtime.ConfirmTeamNonInteractive(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.ErrorIs(t, err, ErrTeamInteractionRequired)
	assertNoTeamResources(t, runtime.paths)
	state, id := runtime.teamGuard.snapshot()
	assert.Equal(t, teamGuardProposed, state)
	assert.Empty(t, id)

	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, reference.TeamID)
}

func TestTeamConfirmationRequiresExactWorkspaceStateAndAdmission(t *testing.T) {
	t.Parallel()

	t.Run("dirty requires HEAD-only", func(t *testing.T) {
		t.Parallel()

		runtime := openGitTestRuntime(t, newRuntimeModel(), nil)
		require.NoError(t, os.WriteFile(
			filepath.Join(runtime.workspace.Root(), "README.md"),
			[]byte("changed\n"),
			0o600,
		))

		proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
		require.NoError(t, err)
		assert.True(t, proposal.Dirty)

		_, err = runtime.ConfirmTeam(t.Context(), TeamConfirmation{
			ProposalID: proposal.ID, Admission: TeamAdmissionClean,
		})
		assert.ErrorIs(t, err, ErrTeamDirty)
		assertNoTeamResources(t, runtime.paths)
	})

	t.Run("proposal becomes stale", func(t *testing.T) {
		t.Parallel()

		runtime := openGitTestRuntime(t, newRuntimeModel(), nil)
		proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			filepath.Join(runtime.workspace.Root(), "new.txt"),
			[]byte("drift\n"),
			0o600,
		))

		_, err = runtime.ConfirmTeam(t.Context(), TeamConfirmation{
			ProposalID: proposal.ID, Admission: TeamAdmissionClean,
		})
		assert.ErrorIs(t, err, ErrTeamProposalStale)
		assertNoTeamResources(t, runtime.paths)
	})

	t.Run("proposal expires", func(t *testing.T) {
		t.Parallel()

		runtime := openGitTestRuntime(t, newRuntimeModel(), nil)
		proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
		require.NoError(t, err)
		runtime.admission.now = func() time.Time { return proposal.ExpiresAt }

		_, err = runtime.ConfirmTeam(t.Context(), TeamConfirmation{
			ProposalID: proposal.ID, Admission: TeamAdmissionClean,
		})
		assert.ErrorIs(t, err, ErrTeamProposalStale)
		assertNoTeamResources(t, runtime.paths)
	})
}

func TestTeamConfirmationPersistsAggregateAndNarrowsLeadCatalog(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("coordinating"))
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)

	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, reference.TeamID)
	assert.Equal(t, TeamAdmissionClean, reference.Admission)
	assert.Equal(t, "lead", string(reference.LeadMember))

	stored, err := runtime.team.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	assert.Equal(t, teamstate.StateActive, stored.State)
	assert.Equal(t, reference.BaseOID, stored.Repository.BaseOID)
	require.Len(t, stored.Members, 2)

	aggregate, err := runtime.team.engine.Get(t.Context(), reference.TeamID)
	require.NoError(t, err)
	require.Len(t, aggregate.Members, 2)
	require.Len(t, aggregate.Tasks, 1)
	assert.Equal(t, "task-1", string(aggregate.Tasks[0].ID))
	require.NotNil(t, runtime.team.lease)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("coordinate the Team")))
	requests := model.Requests()
	require.Len(t, requests, 1)
	names := toolNamesFromRequest(requests[0])
	for _, expected := range []string{
		"read", "ls", "glob", "grep", "ask_user", "team_get_status",
		"team_list_tasks", "team_send_message", "team_create_task",
		"team_assign_task", "team_complete",
	} {
		assert.Contains(t, names, expected)
	}
	for _, forbidden := range []string{
		"apply_patch", "shell", "write_plan", "run_subagent", "spawn_agent",
		"team_claim_task", "team_release_task", "team_finish_task_attempt",
	} {
		assert.NotContains(t, names, forbidden)
	}
	assert.ErrorIs(t, runtime.SetMode(t.Context(), ModePlan), ErrTeamActive)
	require.ErrorIs(t, runtime.ReplacementPreflight(t.Context()), ErrTeamActive)
	assert.ErrorIs(t, runtime.Reload(t.Context()), ErrTeamActive)
}

func TestTeamLifecycleProjectsCompactParentStateAndTerminalUsageOnce(t *testing.T) {
	t.Parallel()

	runtime := openGitTestRuntime(t, newRuntimeModel(runtimeTextResponse("worker complete")), nil)
	var (
		mu     sync.Mutex
		events []TelemetryEvent
	)
	runtime.telemetry = newTelemetryObservers([]TelemetryObserver{TelemetryObserverFunc(
		func(_ context.Context, event TelemetryEvent) error {
			if event.Type == EventTeamLifecycle {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			}

			return nil
		},
	)})

	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		return getErr == nil && len(aggregate.Tasks) == 1 &&
			aggregate.Tasks[0].Status == team.TaskStatusCompleted
	}, 30*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		state := runtime.Snapshot()
		for _, value := range state.Teams {
			if value.TeamID == reference.TeamID && value.AttemptID != "" &&
				value.State == TeamLifecycleCompleted {
				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond)

	mu.Lock()
	observed := append([]TelemetryEvent(nil), events...)
	mu.Unlock()
	states := make([]string, 0, len(observed))
	terminalAttempts := 0
	for _, event := range observed {
		states = append(states, event.TeamState)
		terminal := event.TeamState == string(TeamLifecycleCompleted) ||
			event.TeamState == string(TeamLifecycleFailed) ||
			event.TeamState == string(TeamLifecycleCancelled) ||
			event.TeamState == string(TeamLifecycleInterrupted)
		if terminal {
			terminalAttempts++
		}
		if !terminal {
			assert.Equal(t, TokenUsage{}, event.Usage)
			assert.Zero(t, event.DurationMillis)
		}
	}
	assert.Contains(t, states, string(TeamLifecycleProposed))
	assert.Contains(t, states, string(TeamLifecycleAdmitted))
	assert.Contains(t, states, string(TeamLifecycleWaiting))
	assert.Contains(t, states, string(TeamLifecycleRunning))
	assert.Contains(t, states, string(TeamLifecycleCapturing))
	assert.Contains(t, states, string(TeamLifecycleCompleted))
	assert.Equal(t, 1, terminalAttempts)
}

func TestTeamControlCancelTeamIsJournaledBeforeApplication(t *testing.T) {
	t.Parallel()

	runtime := openGitTestRuntime(t, newRuntimeModel(runtimeTextResponse("worker complete")), nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)

	control, err := runtime.SubmitTeamControl(t.Context(), TeamControlRequest{
		TeamID: reference.TeamID, Action: TeamControlCancelTeam,
	})
	require.NoError(t, err)
	assert.Equal(t, reference.TeamID, control.TeamID)
	require.Eventually(t, func() bool {
		record, getErr := runtime.team.control.Get(t.Context(), reference.TeamID, control.CommandID)
		if getErr != nil || record.Entry.State != teamcontrol.StateApplied {
			return false
		}
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)

		return getErr == nil && aggregate.Status == team.StatusCancelled
	}, 30*time.Second, 10*time.Millisecond)
}

func TestTeamControlStaleTargetHasNoDomainSideEffect(t *testing.T) {
	t.Parallel()

	runtime := openGitTestRuntime(t, newRuntimeModel(runtimeTextResponse("worker complete")), nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)

	control, err := runtime.SubmitTeamControl(t.Context(), TeamControlRequest{
		TeamID: reference.TeamID, Action: TeamControlCancelTask, TaskID: "missing-task",
	})
	require.NoError(t, err)
	var latest teamcontrol.Record
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		record, getErr := runtime.team.control.Get(t.Context(), reference.TeamID, control.CommandID)
		require.NoError(collect, getErr)
		latest = record
		assert.Equal(collect, teamcontrol.StateStale, record.Entry.State)
		assert.Nil(collect, record.Entry.Resolved)
	}, 3*time.Second, 10*time.Millisecond)
	assert.Equal(t, "stale_target", latest.Entry.ErrorCode)
	aggregate, err := runtime.team.engine.Get(t.Context(), reference.TeamID)
	require.NoError(t, err)
	assert.Equal(t, team.StatusActive, aggregate.Status)
}

func TestTeamControlMessageUsesExactOwnerAndMailboxCursor(t *testing.T) {
	t.Parallel()

	model := newTeamControlFollowUpModel("review the failing assertion")
	t.Cleanup(model.unblock)
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	model.waitStarted(t)

	aggregate, err := runtime.team.engine.Get(t.Context(), reference.TeamID)
	require.NoError(t, err)
	require.Len(t, aggregate.Tasks, 1)
	require.Len(t, aggregate.Tasks[0].Attempts, 1)
	attempt := aggregate.Tasks[0].Attempts[0]
	control, err := runtime.SubmitTeamControl(t.Context(), TeamControlRequest{
		TeamID: reference.TeamID, Action: TeamControlMessage,
		MemberID: attempt.MemberID, TaskID: aggregate.Tasks[0].ID,
		ExpectedAttemptID: attempt.ID, Text: model.followUps[0],
	})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		record, getErr := runtime.team.control.Get(t.Context(), reference.TeamID, control.CommandID)
		require.NoError(collect, getErr)
		assert.Equal(collect, teamcontrol.StateApplied, record.Entry.State)
		current, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		require.NoError(collect, getErr)
		for _, member := range current.Members {
			if member.ID == attempt.MemberID {
				assert.Equal(collect, uint64(1), member.MailboxAcknowledged)
				return
			}
		}
		collect.Errorf("Worker member %s is missing", attempt.MemberID)
	}, 3*time.Second, 10*time.Millisecond)

	model.unblock()
	require.Eventually(t, func() bool {
		return model.callCount() == 2
	}, 30*time.Second, 10*time.Millisecond)
}

func TestTeamControlSerializesMultipleFollowUpsWithoutLoss(t *testing.T) {
	t.Parallel()

	model := newTeamControlFollowUpModel("inspect parser", "run focused tests")
	t.Cleanup(model.unblock)
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	model.waitStarted(t)

	aggregate, err := runtime.team.engine.Get(t.Context(), reference.TeamID)
	require.NoError(t, err)
	require.Len(t, aggregate.Tasks, 1)
	require.Len(t, aggregate.Tasks[0].Attempts, 1)
	attempt := aggregate.Tasks[0].Attempts[0]
	references := make([]TeamControlReference, 0, len(model.followUps))
	for _, followUp := range model.followUps {
		control, submitErr := runtime.SubmitTeamControl(t.Context(), TeamControlRequest{
			TeamID: reference.TeamID, Action: TeamControlFollowUp,
			MemberID: attempt.MemberID, TaskID: aggregate.Tasks[0].ID,
			ExpectedAttemptID: attempt.ID, Text: followUp,
		})
		require.NoError(t, submitErr)
		references = append(references, control)
	}
	for _, control := range references {
		require.Eventually(t, func() bool {
			record, getErr := runtime.team.control.Get(
				t.Context(), reference.TeamID, control.CommandID,
			)

			return getErr == nil && record.Entry.State == teamcontrol.StateApplied
		}, 30*time.Second, 10*time.Millisecond)
	}

	model.unblock()
	require.Eventually(t, func() bool {
		return model.callCount() == 1+len(model.followUps)
	}, 30*time.Second, 10*time.Millisecond)
}

func TestTeamControlInterruptStopsExactRunningAttempt(t *testing.T) {
	t.Parallel()

	model := newTeamControlFollowUpModel()
	t.Cleanup(model.unblock)
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	model.waitStarted(t)

	aggregate, err := runtime.team.engine.Get(t.Context(), reference.TeamID)
	require.NoError(t, err)
	require.Len(t, aggregate.Tasks, 1)
	require.Len(t, aggregate.Tasks[0].Attempts, 1)
	attempt := aggregate.Tasks[0].Attempts[0]
	control, err := runtime.SubmitTeamControl(t.Context(), TeamControlRequest{
		TeamID: reference.TeamID, Action: TeamControlInterruptAttempt,
		MemberID: attempt.MemberID, TaskID: aggregate.Tasks[0].ID,
		ExpectedAttemptID: attempt.ID,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		record, getErr := runtime.team.control.Get(
			t.Context(), reference.TeamID, control.CommandID,
		)

		return getErr == nil && record.Entry.State == teamcontrol.StateApplied
	}, 30*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		current, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil || len(current.Tasks) != 1 {
			return false
		}

		return current.Tasks[0].Status == team.TaskStatusFailed
	}, 30*time.Second, 20*time.Millisecond)
}

func TestTeamCaptureRecoversPublishedResultBeforeResourceCommit(t *testing.T) {
	t.Parallel()

	model := newTeamControlFollowUpModel()
	t.Cleanup(model.unblock)
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	model.waitStarted(t)

	owner := waitForRunningAttemptOwner(t, runtime)
	owner.mu.Lock()
	resource := owner.resource
	owner.mu.Unlock()
	require.NotEmpty(t, resource.ID)
	assert.Empty(t, resource.ResultCommitOID)
	published, err := runtime.team.worktree.Capture(
		t.Context(), runtime.team.lease, resource,
		teamworktree.CaptureRequest{Message: "simulate capture crash window"},
	)
	require.NoError(t, err)
	require.NotEmpty(t, published.CommitOID)

	recovered, err := owner.worktreeForProjection(t.Context())
	require.NoError(t, err)
	assert.Equal(t, published.CommitOID, recovered.ResultCommitOID)
	snapshot, err := runtime.team.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	require.Len(t, snapshot.Attempts, 1)
	assert.Equal(t, published.CommitOID, snapshot.Attempts[0].Worktree.ResultCommitOID)
	assert.Equal(t, teamstate.AttemptCapturing, snapshot.Attempts[0].State)

	require.NoError(t, owner.deliver(t.Context(), ownerCommand{kind: ownerCommandInterrupt}))
	require.Eventually(t, func() bool {
		current, getErr := runtime.team.state.Load(t.Context(), reference.TeamID)

		return getErr == nil && len(current.Attempts) == 1 &&
			current.Attempts[0].State == teamstate.AttemptTerminal &&
			current.Attempts[0].Worktree.ResultCommitOID == published.CommitOID
	}, 30*time.Second, 10*time.Millisecond)
}

func TestParallelTeamWorkersCaptureIndependentResultsWithoutParentWrites(t *testing.T) {
	t.Parallel()

	model := newParallelTeamWriteModel("worker-a.txt", "worker-b.txt")
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), TeamProposalRequest{
		Objective: "Create two independent result files without modifying the parent Workspace.",
		Workers: []TeamWorkerSpec{
			{Name: "Worker A", Role: "Create worker-a.txt."},
			{Name: "Worker B", Role: "Create worker-b.txt."},
		},
		Tasks: []TeamTaskSpec{
			{ID: "task-a", Title: "Create worker-a.txt", AssignedWorker: "Worker A"},
			{ID: "task-b", Title: "Create worker-b.txt", AssignedWorker: "Worker B"},
		},
	})
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)

	completed := assert.Eventually(t, func() bool {
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil || len(aggregate.Tasks) != 2 {
			return false
		}
		for _, task := range aggregate.Tasks {
			if task.Status != team.TaskStatusCompleted {
				return false
			}
		}

		return true
	}, 30*time.Second, 20*time.Millisecond)
	if !completed {
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		runtime.team.mu.Lock()
		coordinatorErr := runtime.team.closeErr
		runtime.team.mu.Unlock()
		require.FailNow(t, "parallel Team did not complete", "Team=%+v load=%v coordinator=%v", aggregate, getErr, coordinatorErr)
	}
	var snapshot teamstate.Snapshot
	require.Eventually(t, func() bool {
		var loadErr error
		snapshot, loadErr = runtime.team.state.Load(t.Context(), reference.TeamID)
		if loadErr != nil || len(snapshot.Attempts) != 2 {
			return false
		}
		for _, attempt := range snapshot.Attempts {
			if attempt.State != teamstate.AttemptTerminal {
				return false
			}
		}

		return true
	}, 30*time.Second, 10*time.Millisecond)
	require.Len(t, snapshot.Attempts, 2)
	commits := make(map[string]struct{}, len(snapshot.Attempts))
	for _, attempt := range snapshot.Attempts {
		assert.Equal(t, teamstate.AttemptTerminal, attempt.State)
		require.NotEmpty(t, attempt.Worktree.ResultCommitOID)
		commits[attempt.Worktree.ResultCommitOID] = struct{}{}
		name := map[team.TaskID]string{"task-a": "worker-a.txt", "task-b": "worker-b.txt"}[attempt.TaskID]
		_, statErr := os.Stat(filepath.Join(attempt.Worktree.Directory.Path, name))
		require.NoError(t, statErr)
	}
	assert.Len(t, commits, 2)
	for _, name := range []string{"worker-a.txt", "worker-b.txt"} {
		_, statErr := os.Stat(filepath.Join(runtime.workspace.Root(), name))
		assert.ErrorIs(t, statErr, os.ErrNotExist)
	}
}

func TestTeamWorkerQuestionResolutionBindsAttemptAndOwnerGeneration(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeQuestionResponse(t, "call-worker-question"),
		runtimeTextResponse("continued with user answer"),
	)
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)

	target, state := waitForPausedTeamWorker(t, runtime, reference.TeamID)
	require.NotNil(t, state.Question.Required)
	request := question.CloneRequest(*state.Question.Required)

	staleTarget := target
	staleTarget.OwnerGeneration++
	stale, err := runtime.ResolveTeamWorkerQuestion(t.Context(), staleTarget, question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		record, getErr := runtime.team.control.Get(t.Context(), target.TeamID, stale.CommandID)

		return getErr == nil && record.Entry.State == teamcontrol.StateStale
	}, 30*time.Second, 20*time.Millisecond)

	resolved, err := runtime.ResolveTeamWorkerQuestion(t.Context(), target, question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		record, getErr := runtime.team.control.Get(t.Context(), target.TeamID, resolved.CommandID)

		return getErr == nil && record.Entry.State == teamcontrol.StateApplied
	}, 30*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		return len(model.Requests()) == 2
	}, 30*time.Second, 20*time.Millisecond)
	assert.True(t, requestContainsToolText(model.Requests()[1], "React"))
}

func TestTeamWorkerApprovalResolutionCannotDriftToAnotherOwner(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse(
			"call-worker-shell",
			"shell",
			`{"command":"printf should-not-run","permissions":{"write_paths":[],"network":true},"justification":"test"}`,
		),
		runtimeTextResponse("continued after denial"),
	)
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)

	target, state := waitForPausedTeamWorker(t, runtime, reference.TeamID)
	require.NotNil(t, state.Approval.Required)
	resolved, err := runtime.ResolveTeamWorkerApproval(t.Context(), target, approval.Resolution{
		RequestID: state.Approval.Required.RequestID, Choice: approval.ChoiceDeny,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		record, getErr := runtime.team.control.Get(t.Context(), target.TeamID, resolved.CommandID)

		return getErr == nil && record.Entry.State == teamcontrol.StateApplied
	}, 30*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		return len(model.Requests()) == 2
	}, 30*time.Second, 20*time.Millisecond)
}

func waitForPausedTeamWorker(
	t *testing.T,
	runtime *Runtime,
	teamID team.ID,
) (TeamWorkerTarget, State) {
	t.Helper()

	var target TeamWorkerTarget
	var state State
	require.Eventually(t, func() bool {
		runtime.team.mu.Lock()
		var owner *attemptOwner
		for _, slot := range runtime.team.owners {
			owner, _ = slot.owner.(*attemptOwner)
			if owner != nil {
				break
			}
		}
		runtime.team.mu.Unlock()
		if owner == nil {
			return false
		}
		owner.mu.Lock()
		workerRuntime := owner.runtime
		owner.mu.Unlock()
		if workerRuntime == nil {
			return false
		}
		state = workerRuntime.Snapshot()
		if state.Phase != PhasePaused ||
			(state.Approval.Required == nil && state.Question.Required == nil) {
			return false
		}
		resources, err := runtime.team.state.Load(t.Context(), teamID)
		if err != nil {
			return false
		}
		for _, resource := range resources.Attempts {
			if resource.AttemptID != owner.candidate.key.attemptID {
				continue
			}
			target = TeamWorkerTarget{
				TeamID: teamID, MemberID: owner.candidate.memberID,
				TaskID: owner.candidate.key.taskID, AttemptID: owner.candidate.key.attemptID,
				OwnerGeneration: resource.Worktree.LeaseGeneration,
			}

			return target.OwnerGeneration != 0
		}

		return false
	}, 30*time.Second, 20*time.Millisecond)

	return target, state
}

func waitForRunningAttemptOwner(t *testing.T, runtime *Runtime) *attemptOwner {
	t.Helper()

	var owner *attemptOwner
	require.Eventually(t, func() bool {
		runtime.team.mu.Lock()
		defer runtime.team.mu.Unlock()
		for _, slot := range runtime.team.owners {
			owner, _ = slot.owner.(*attemptOwner)
			if owner != nil {
				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond)

	return owner
}

func TestLeadGuardClosesAlreadyLeasedInteractionCapabilities(t *testing.T) {
	t.Parallel()

	workspacePath := t.TempDir()
	ws, err := workspace.Open(workspacePath)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(ws)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tree.Close() })
	local, err := tools.NewCatalog(tree, tools.DefaultLimits())
	require.NoError(t, err)
	descriptors, err := local.Search(t.Context(), catalog.AllowAll("test", catalog.RiskPrivileged), "")
	require.NoError(t, err)

	guard := &teamCapabilityGuard{}
	hook := guard.beforeTool(descriptors)
	assert.Equal(t, agent.ToolDecisionAllow, hook(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "apply_patch"},
	}).Action)
	require.NoError(t, guard.propose())
	require.NoError(t, guard.activate("team-1"))
	assert.Equal(t, agent.ToolDecisionDeny, hook(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "apply_patch"},
	}).Action)
	assert.Equal(t, agent.ToolDecisionAllow, hook(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "read"},
	}).Action)
}

func validTeamProposalRequest() TeamProposalRequest {
	return TeamProposalRequest{
		Objective: "Implement the approved change without modifying the parent Workspace.",
		Workers:   []TeamWorkerSpec{{Name: "Implementer", Role: "Implement and verify the task."}},
		Tasks: []TeamTaskSpec{{
			ID: "task-1", Title: "Implement the change",
			Description:    "Make the bounded implementation and run focused tests.",
			AssignedWorker: "Implementer",
		}},
	}
}

func openGitTestRuntime(
	t *testing.T,
	model ai.LanguageModel,
	probe func(context.Context, *execution.Executor) error,
) *Runtime {
	t.Helper()

	base := t.TempDir()
	workspacePath := filepath.Join(base, "workspace")
	require.NoError(t, mkdirPrivate(workspacePath))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "README.md"), []byte("base\n"), 0o600))
	ws, err := workspace.Open(workspacePath)
	require.NoError(t, err)
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	sandbox := config.SandboxFullAccess
	loaded, err := config.Load(config.LoadOptions{
		ConfigFile: filepath.Join(base, "absent-test-config.toml"),
		FlagOverrides: config.Patch{
			Sandbox: &sandbox,
		},
	})
	require.NoError(t, err)
	cfg := loaded.Config
	cfg.Model.Provider = model.Provider()
	cfg.Model.Model = model.ModelID()
	if probe == nil {
		probe = func(context.Context, *execution.Executor) error { return nil }
	}

	runtime, err := Open(t.Context(), OpenOptions{
		Workspace: ws, Trusted: true, Config: cfg, Paths: layout, Model: model,
		Execution: ExecutionOptions{SandboxProbe: probe},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runGitTestCommand(t, runtime, "init")
	runGitTestCommand(t, runtime, "config", "user.name", "Pips Test")
	runGitTestCommand(t, runtime, "config", "user.email", "pips@example.invalid")
	runGitTestCommand(t, runtime, "add", "README.md")
	runGitTestCommand(t, runtime, "commit", "-m", "initial")

	return runtime
}

type teamControlFollowUpModel struct {
	mu        sync.Mutex
	started   chan struct{}
	release   chan struct{}
	once      sync.Once
	calls     int
	followUps []string
}

func newTeamControlFollowUpModel(followUps ...string) *teamControlFollowUpModel {
	return &teamControlFollowUpModel{
		started: make(chan struct{}), release: make(chan struct{}),
		followUps: append([]string(nil), followUps...),
	}
}

func (m *teamControlFollowUpModel) Generate(
	ctx context.Context,
	request ai.Request,
) (*ai.Response, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()

	switch call {
	case 1:
		close(m.started)
		select {
		case <-m.release:
			return runtimeTextResponse("initial work complete"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
		followUpIndex := call - 2
		if followUpIndex < 0 || followUpIndex >= len(m.followUps) {
			return nil, errors.New("Team control model script exhausted")
		}
		if !requestContainsText(request, m.followUps[followUpIndex]) {
			return nil, errors.New("Team control follow-up is missing from model context")
		}

		return runtimeTextResponse("follow-up complete"), nil
	}
}

func (m *teamControlFollowUpModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}
		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (*teamControlFollowUpModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*teamControlFollowUpModel) ModelID() string       { return "team-control-test" }
func (*teamControlFollowUpModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func (m *teamControlFollowUpModel) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-m.started:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "timed out waiting for Team Worker model request")
	}
}

func (m *teamControlFollowUpModel) unblock() { m.once.Do(func() { close(m.release) }) }

func (m *teamControlFollowUpModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls
}

var _ ai.LanguageModel = (*teamControlFollowUpModel)(nil)

type parallelTeamWriteModel struct {
	mu    sync.Mutex
	paths []string
	calls map[string]int
}

func newParallelTeamWriteModel(paths ...string) *parallelTeamWriteModel {
	return &parallelTeamWriteModel{
		paths: append([]string(nil), paths...), calls: make(map[string]int, len(paths)),
	}
}

func (m *parallelTeamWriteModel) Generate(
	_ context.Context,
	request ai.Request,
) (*ai.Response, error) {
	path := ""
	for _, candidate := range m.paths {
		for _, message := range request.Messages {
			if _, isUser := message.(ai.UserMessage); isUser && strings.Contains(runtimeMessageText(message), candidate) {
				path = candidate
				break
			}
		}
		if path != "" {
			break
		}
	}
	if path == "" {
		return nil, errors.New("parallel Team model could not identify Worker task")
	}

	m.mu.Lock()
	m.calls[path]++
	call := m.calls[path]
	m.mu.Unlock()
	if call == 1 {
		arguments, err := json.Marshal(struct {
			Patch string `json:"patch"`
		}{Patch: "*** Begin Patch\n*** Add File: " + path + "\n+captured by " + path + "\n*** End Patch"})
		if err != nil {
			return nil, err
		}

		return runtimeToolResponse("write-"+path, "apply_patch", string(arguments)), nil
	}
	if call == 2 {
		return runtimeTextResponse("Worker result is ready for capture."), nil
	}

	return nil, errors.New("parallel Team model script exhausted")
}

func (m *parallelTeamWriteModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}
		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (*parallelTeamWriteModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*parallelTeamWriteModel) ModelID() string       { return "parallel-team-test" }
func (*parallelTeamWriteModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = (*parallelTeamWriteModel)(nil)

func runGitTestCommand(t *testing.T, runtime *Runtime, args ...string) {
	t.Helper()

	operation, err := execution.NewOperation(t.Context(), runtime.workspace, execution.OperationSpec{
		Kind: execution.KindGit, Tool: "team_admission_test", Executable: runtime.opts.GitPath,
		Args: args, CWD: ".", Timeout: 30 * time.Second,
		Output: execution.OutputLimits{
			CaptureBytes: 1 << 20, MaxBytes: 2 << 20, ChunkBytes: 4096, QueueDepth: 4,
		},
		Workspace: execution.WorkspaceWrite, Network: execution.NetworkNone,
	})
	require.NoError(t, err)
	authorization, err := runtime.policy.Approve(operation)
	require.NoError(t, err)
	result, err := runtime.executor.Execute(t.Context(), operation, authorization, nil)
	require.NoError(t, err)
	require.Equal(t, execution.StatusExited, result.Status)
	require.Zero(t, result.ExitCode, string(result.Stderr.Head()))
}

func assertNoTeamResources(t *testing.T, layout paths.Layout) {
	t.Helper()

	for _, path := range []string{layout.TeamsDir(), layout.WorktreesRoot()} {
		_, err := os.Lstat(path)
		assert.ErrorIs(t, err, os.ErrNotExist, path)
	}
}
