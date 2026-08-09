//nolint:wsl_v5 // Recovery fixtures keep durable lifecycle steps beside their assertions.
package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordLimitsRoundTripUnlimitedDefaultsAndAcceptLegacyOmissions(t *testing.T) {
	t.Parallel()

	current := journalLimits(DefaultLimits())
	data, err := json.Marshal(current)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"max_turns":0`)
	assert.Contains(t, string(data), `"max_tokens":0`)
	assert.Contains(t, string(data), `"max_tool_calls":0`)
	assert.Contains(t, string(data), `"max_duration_nanos":0`)

	var decoded recordLimits
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, current, decoded)
	require.NoError(t, decoded.validate())

	legacy := legacyBoundedRecordLimits()
	require.NoError(t, legacy.validate())
	normalized := normalizeLimits(legacy.limits())
	assert.Equal(t, 1, normalized.FinalizationTurns)
	assert.Equal(t, DefaultLimits().RepeatedToolCallLimit, normalized.RepeatedToolCallLimit)
	assert.Equal(t, DefaultLimits().MaxActivityTools, normalized.MaxActivityTools)
}

func TestReconcileMarksOrphanRunningInterruptedAndRepairsParent(t *testing.T) {
	t.Parallel()

	repository, parent, workspaceID := newJournalFixture(t)
	child, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: workspaceID, WorkspacePath: parent.Metadata().WorkspacePath,
		Kind:            session.KindSubagent,
		ParentSessionID: parent.Metadata().ID, ParentRunID: "parent-run",
		Agent: string(RoleExplore),
	})
	require.NoError(t, err)

	created := record{
		Schema: recordSchema, State: StateCreated, Role: RoleExplore,
		ChildSessionID: child.Metadata().ID, ParentSessionID: parent.Metadata().ID,
		ParentRunID: "parent-run", Model: "openai/test", TaskPreview: "inspect",
		Limits: legacyBoundedRecordLimits(), Time: time.Now().UTC(),
	}
	require.NoError(t, appendMirrored(child.Session(), parent.Session(), created))
	started := created
	started.State = StateRunning
	started.ChildRunID = "child-run"
	started.Time = time.Now().UTC()
	require.NoError(t, appendMirrored(child.Session(), parent.Session(), started))
	require.NoError(t, child.Close())

	require.NoError(t, Reconcile(t.Context(), repository, parent))
	parentLatest, err := latestByChild(parent.Session().Entries())
	require.NoError(t, err)
	assert.Equal(t, StateInterrupted, parentLatest[created.ChildSessionID].State)
	assert.Equal(t, "process_interrupted", parentLatest[created.ChildSessionID].Code)

	require.NoError(t, Reconcile(t.Context(), repository, parent))
	values, err := records(parent.Session().Entries())
	require.NoError(t, err)

	terminalCount := 0

	for _, value := range values {
		if value.ChildSessionID == created.ChildSessionID && value.State == StateInterrupted {
			terminalCount++
		}
	}

	assert.Equal(t, 1, terminalCount)
}

func TestReconcileInterruptsPendingChildInputWithoutReplayingOrResolvingIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		toolName string
		args     ai.JSON
	}{
		{
			name:     "approval",
			toolName: "shell",
			args:     ai.JSON(`{"command":"printf must-not-run","permissions":{"network":true}}`),
		},
		{
			name:     "question",
			toolName: "ask_user",
			args:     ai.JSON(`{"questions":[{"header":"UI","prompt":"Choose","options":[{"label":"A","description":"first"},{"label":"B","description":"second"}]}]}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			repository, parent, workspaceID := newJournalFixture(t)
			child, err := repository.Create(t.Context(), session.CreateOptions{
				WorkspaceID: workspaceID, WorkspacePath: parent.Metadata().WorkspacePath,
				Kind: session.KindSubagent, ParentSessionID: parent.Metadata().ID,
				ParentRunID: "parent-run", Agent: string(RoleExplore),
			})
			require.NoError(t, err)
			created := record{
				Schema: recordSchema, State: StateCreated, Role: RoleExplore,
				ChildSessionID: child.Metadata().ID, ParentSessionID: parent.Metadata().ID,
				ParentRunID: "parent-run", Model: "openai/test", TaskPreview: "pending input",
				Limits: journalLimits(ProductionLimits()), Time: time.Now().UTC(),
			}
			require.NoError(t, appendMirrored(child.Session(), parent.Session(), created))
			started := created
			started.State = StateRunning
			started.ChildRunID = "child-run"
			started.Time = started.Time.Add(time.Millisecond)
			require.NoError(t, appendMirrored(child.Session(), parent.Session(), started))
			const staleCallID = "stale-child-call"
			_, err = child.Session().AppendMessage(ai.Assistant(ai.ToolCallPart{
				ID: staleCallID, Name: test.toolName, Args: test.args,
			}), nil)
			require.NoError(t, err)
			require.NoError(t, child.Close())

			require.NoError(t, Reconcile(t.Context(), repository, parent))
			require.NoError(t, Reconcile(t.Context(), repository, parent))
			reopened, err := repository.Open(t.Context(), session.OpenOptions{
				ID: created.ChildSessionID, WorkspaceID: workspaceID,
			})
			require.NoError(t, err)
			pending, err := reopened.Session().Pending()
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, staleCallID, pending[0].ID)
			assert.Equal(t, test.toolName, pending[0].Name)
			childRecords, err := records(reopened.Session().Entries())
			require.NoError(t, err)
			require.NoError(t, reopened.Close())
			terminalCount := 0
			for _, value := range childRecords {
				if value.State == StateInterrupted {
					terminalCount++
					assert.Equal(t, "process_interrupted", value.Code)
				}
			}
			assert.Equal(t, 1, terminalCount)

			parentRecords, err := recordsByChild(parent.Session().Entries())
			require.NoError(t, err)
			assert.Equal(t, childRecords, parentRecords[created.ChildSessionID])
		})
	}
}

func TestReconcileRepairsRecursiveDescendantsDeepestFirstAndIdempotently(t *testing.T) {
	t.Parallel()

	repository, parent, workspaceID := newJournalFixture(t)
	rootPlan := recursiveJournalPlan(t, "root-agent", 0, []string{"root-agent"}, []string{"middle-agent"})
	middlePlan := recursiveJournalPlan(
		t, "middle-agent", 1, []string{"root-agent", "middle-agent"}, []string{"leaf-agent"},
	)
	leafPlan := recursiveJournalPlan(
		t, "leaf-agent", 2, []string{"root-agent", "middle-agent", "leaf-agent"}, nil,
	)
	rootChild, rootRecord := createRunningJournalChild(t, repository, parent, workspaceID, rootPlan, "root-run")
	middleChild, middleRecord := createRunningJournalChild(
		t, repository, rootChild, workspaceID, middlePlan, "middle-run",
	)
	leafChild, leafRecord := createRunningJournalChild(
		t, repository, middleChild, workspaceID, leafPlan, "leaf-run",
	)
	require.NoError(t, leafChild.Close())
	require.NoError(t, middleChild.Close())
	require.NoError(t, rootChild.Close())

	require.NoError(t, Reconcile(t.Context(), repository, parent))
	times := make(map[string]time.Time, 3)
	entryCounts := make(map[string]int, 3)
	for _, expected := range []record{rootRecord, middleRecord, leafRecord} {
		handle, err := repository.Open(t.Context(), session.OpenOptions{
			ID: expected.ChildSessionID, WorkspaceID: workspaceID,
		})
		require.NoError(t, err)
		values, err := authoritativeChildRecords(handle)
		require.NoError(t, err)
		terminal := values[len(values)-1]
		assert.Equal(t, StateInterrupted, terminal.State)
		assert.Equal(t, "process_interrupted", terminal.Code)
		times[expected.Identity.ID] = terminal.Time
		entryCounts[expected.ChildSessionID] = len(handle.Session().Entries())
		require.NoError(t, handle.Close())
	}
	assert.False(t, times["leaf-agent"].After(times["middle-agent"]))
	assert.False(t, times["middle-agent"].After(times["root-agent"]))

	require.NoError(t, Reconcile(t.Context(), repository, parent))
	for childSessionID, count := range entryCounts {
		handle, err := repository.Open(t.Context(), session.OpenOptions{
			ID: childSessionID, WorkspaceID: workspaceID,
		})
		require.NoError(t, err)
		assert.Len(t, handle.Session().Entries(), count)
		require.NoError(t, handle.Close())
	}
}

func TestReconcileRejectsRecursiveAuthorityLineageBeforeTerminalizing(t *testing.T) {
	t.Parallel()

	repository, parent, workspaceID := newJournalFixture(t)
	rootPlan := recursiveJournalPlan(t, "root-agent", 0, []string{"root-agent"}, []string{"other-agent"})
	middlePlan := recursiveJournalPlan(
		t, "middle-agent", 1, []string{"root-agent", "middle-agent"}, nil,
	)
	rootChild, _ := createRunningJournalChild(t, repository, parent, workspaceID, rootPlan, "root-run")
	middleChild, middleRecord := createRunningJournalChild(
		t, repository, rootChild, workspaceID, middlePlan, "middle-run",
	)
	require.NoError(t, middleChild.Close())
	require.NoError(t, rootChild.Close())

	err := Reconcile(t.Context(), repository, parent)
	require.ErrorIs(t, err, ErrInvalid)
	reopened, err := repository.Open(t.Context(), session.OpenOptions{
		ID: middleRecord.ChildSessionID, WorkspaceID: workspaceID,
	})
	require.NoError(t, err)
	values, err := authoritativeChildRecords(reopened)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, values[len(values)-1].State)
	require.NoError(t, reopened.Close())
}

func recursiveJournalPlan(
	t *testing.T,
	id string,
	depth int,
	ancestry []string,
	targets []string,
) ExecutionPlan {
	t.Helper()
	plan := testDelegationExecutionPlan(t)
	plan.Identity.ID = id
	plan.Identity.Name = id
	plan.Identity.DefinitionDigest = strings.Repeat([]string{"a", "b", "c"}[depth], 64)
	plan.DelegationDepth = depth
	plan.MaxDelegationDepth = 2
	plan.Ancestry = slices.Clone(ancestry)
	plan.DelegationTargets = slices.Clone(targets)
	require.NoError(t, ValidateExecutionPlan(plan))

	return plan
}

func createRunningJournalChild(
	t *testing.T,
	repository *session.Repository,
	parent *session.Handle,
	workspaceID string,
	plan ExecutionPlan,
	parentRunID string,
) (*session.Handle, record) {
	t.Helper()
	digest, err := plan.Digest()
	require.NoError(t, err)
	identity := plan.Identity
	child, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: workspaceID, WorkspacePath: parent.Metadata().WorkspacePath,
		Kind: session.KindSubagent, ParentSessionID: parent.Metadata().ID,
		ParentRunID: parentRunID, Agent: identity.ID,
		SubagentIdentity: &session.SubagentIdentity{
			Schema: identity.Schema, AgentID: identity.ID, Kind: string(identity.Kind), Name: identity.Name,
			DefinitionSchema: identity.DefinitionSchema, DefinitionDigest: identity.DefinitionDigest,
			DefinitionSource: identity.DefinitionSource, GenerationID: plan.GenerationID, PlanDigest: digest,
		},
	})
	require.NoError(t, err)
	created := record{
		Schema: recordSchema, State: StateCreated, Identity: identity, Plan: plan,
		ChildSessionID: child.Metadata().ID, ParentSessionID: parent.Metadata().ID,
		ParentInteractionID: "root-interaction", ParentRunID: parentRunID,
		ParentToolCallID: "call-" + identity.ID, RootInteractionID: "root-interaction",
		Delivery: DeliveryForeground, Model: plan.Model, Limits: journalLimits(plan.Limits),
		TaskPreview: "recursive recovery", Time: time.Now().UTC(),
	}
	require.NoError(t, appendMirrored(child.Session(), parent.Session(), created))
	started := created
	started.State = StateRunning
	started.ChildRunID = "run-" + identity.ID
	started.Time = started.Time.Add(time.Millisecond)
	require.NoError(t, appendMirrored(child.Session(), parent.Session(), started))

	return child, created
}

func TestReconcileRejectsHeaderPlanLineageMismatchWithoutTerminalizing(t *testing.T) {
	t.Parallel()

	repository, parent, workspaceID := newJournalFixture(t)
	child, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: workspaceID, WorkspacePath: parent.Metadata().WorkspacePath,
		Kind: session.KindSubagent, ParentSessionID: parent.Metadata().ID,
		Agent: string(RoleExplore),
		SubagentIdentity: &session.SubagentIdentity{
			Schema: "pips.coding.subagent.identity/v1alpha1", AgentID: string(RoleExplore),
			Kind: "builtin", Name: "Explore", DefinitionSchema: "pips.agent/v1alpha1",
			DefinitionDigest: strings.Repeat("a", 64), DefinitionSource: "builtin",
			GenerationID: 1, PlanDigest: strings.Repeat("b", 64),
		},
	})
	require.NoError(t, err)
	created := record{
		Schema: recordSchema, State: StateCreated, Role: RoleExplore,
		ChildSessionID: child.Metadata().ID, ParentSessionID: parent.Metadata().ID,
		Model: "openai/test", Limits: journalLimits(ProductionLimits()), Time: time.Now().UTC(),
	}
	require.NoError(t, appendMirrored(child.Session(), parent.Session(), created))
	require.NoError(t, child.Close())

	err = Reconcile(t.Context(), repository, parent)
	require.ErrorIs(t, err, ErrInvalid)
	parentRecords, recordErr := recordsByChild(parent.Session().Entries())
	require.NoError(t, recordErr)
	require.Len(t, parentRecords[created.ChildSessionID], 1)
	assert.Equal(t, StateCreated, parentRecords[created.ChildSessionID][0].State)
}

func legacyBoundedRecordLimits() recordLimits {
	value := journalLimits(DefaultLimits())
	value.MaxTurns = 26
	value.MaxTokens = 120_000
	value.MaxToolCalls = 64
	value.MaxDurationNanos = int64(5 * time.Minute)
	value.FinalizationTurns = 0
	value.RepeatedToolCallLimit = 0
	value.MaxActivityTools = 0

	return value
}

func TestReconcileRepairsMissingParentTerminalFromChild(t *testing.T) {
	t.Parallel()

	repository, parent, workspaceID := newJournalFixture(t)
	child, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: workspaceID, WorkspacePath: parent.Metadata().WorkspacePath,
		Kind:            session.KindSubagent,
		ParentSessionID: parent.Metadata().ID, ParentRunID: "parent-run",
		Agent: string(RoleReview),
	})
	require.NoError(t, err)

	created := record{
		Schema: recordSchema, State: StateCreated, Role: RoleReview,
		ChildSessionID: child.Metadata().ID, ParentSessionID: parent.Metadata().ID,
		ParentRunID: "parent-run", Model: "openai/test", TaskPreview: "review",
		Limits: journalLimits(DefaultLimits()), Time: time.Now().UTC(),
	}
	require.NoError(t, appendMirrored(child.Session(), parent.Session(), created))
	started := created
	started.State = StateRunning
	started.ChildRunID = "child-run"
	started.Time = time.Now().UTC()
	require.NoError(t, appendRecord(child.Session(), started))
	terminal := started
	terminal.State = StateSucceeded
	terminal.Code = "ok"
	terminal.Time = time.Now().UTC()
	require.NoError(t, appendRecord(child.Session(), terminal))
	require.NoError(t, child.Close())

	require.NoError(t, Reconcile(t.Context(), repository, parent))
	parentLatest, err := latestByChild(parent.Session().Entries())
	require.NoError(t, err)
	assert.Equal(t, StateSucceeded, parentLatest[created.ChildSessionID].State)

	parentRecords, err := recordsByChild(parent.Session().Entries())
	require.NoError(t, err)
	require.Len(t, parentRecords[created.ChildSessionID], 3)
	assert.Equal(t, []State{StateCreated, StateRunning, StateSucceeded}, []State{
		parentRecords[created.ChildSessionID][0].State,
		parentRecords[created.ChildSessionID][1].State,
		parentRecords[created.ChildSessionID][2].State,
	})
}

func TestReconcileRepairsParentMissingEntireChildLifecycle(t *testing.T) {
	t.Parallel()

	repository, parent, workspaceID := newJournalFixture(t)
	child, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: workspaceID, WorkspacePath: parent.Metadata().WorkspacePath,
		Kind:            session.KindSubagent,
		ParentSessionID: parent.Metadata().ID, Agent: string(RolePlan),
	})
	require.NoError(t, err)

	created := record{
		Schema: recordSchema, State: StateCreated, Role: RolePlan,
		ChildSessionID: child.Metadata().ID, ParentSessionID: parent.Metadata().ID,
		Model: "openai/test", Limits: journalLimits(DefaultLimits()),
		TaskPreview: "plan", Time: time.Now().UTC(),
	}
	require.NoError(t, appendRecord(child.Session(), created))
	require.NoError(t, child.Close())

	require.NoError(t, Reconcile(t.Context(), repository, parent))
	parentRecords, err := recordsByChild(parent.Session().Entries())
	require.NoError(t, err)
	require.Len(t, parentRecords[created.ChildSessionID], 2)
	assert.Equal(t, StateCreated, parentRecords[created.ChildSessionID][0].State)
	assert.Equal(t, StateInterrupted, parentRecords[created.ChildSessionID][1].State)

	child, err = repository.Open(t.Context(), session.OpenOptions{
		ID: created.ChildSessionID, WorkspaceID: workspaceID,
	})
	require.NoError(t, err)
	childRecords, err := recordsByChild(child.Session().Entries())
	require.NoError(t, err)
	assert.Equal(t, childRecords[created.ChildSessionID], parentRecords[created.ChildSessionID])
	require.NoError(t, child.Close())
}

func TestLatestByChildRejectsExecutionMetadataDrift(t *testing.T) {
	t.Parallel()

	created := record{
		Schema: recordSchema, State: StateCreated, Role: RoleExplore,
		ChildSessionID: "child", ParentSessionID: "parent", Model: "openai/test",
		Limits: journalLimits(DefaultLimits()), TaskPreview: "inspect", Time: time.Now().UTC(),
	}
	started := created
	started.State = StateRunning
	started.ChildRunID = "run"
	started.Limits.MaxToolCalls++
	started.Time = started.Time.Add(time.Millisecond)

	child, err := harness.NewSession(harness.NewMemoryStore("child"))
	require.NoError(t, err)
	require.NoError(t, appendRecord(child, created))
	require.NoError(t, appendRecord(child, started))
	_, err = latestByChild(child.Entries())
	require.ErrorIs(t, err, ErrInvalid)
}

func newJournalFixture(t *testing.T) (*session.Repository, *session.Handle, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Mkdir(root, 0o700))
	repository, err := session.NewRepository(root)
	require.NoError(t, err)

	workspaceID := "workspace-test"
	parent, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: workspaceID, WorkspacePath: filepath.Dir(root),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, parent.Close()) })

	return repository, parent, workspaceID
}
