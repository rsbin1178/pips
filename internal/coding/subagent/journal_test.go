package subagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcileMarksOrphanRunningInterruptedAndRepairsParent(t *testing.T) {
	t.Parallel()

	repository, parent, workspaceID := newJournalFixture(t)
	child, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: workspaceID, Kind: session.KindSubagent,
		ParentSessionID: parent.Metadata().ID, ParentRunID: "parent-run",
		Agent: string(RoleExplore),
	})
	require.NoError(t, err)

	created := record{
		Schema: recordSchema, State: StateCreated, Role: RoleExplore,
		ChildSessionID: child.Metadata().ID, ParentSessionID: parent.Metadata().ID,
		ParentRunID: "parent-run", Model: "openai/test", TaskPreview: "inspect",
		Limits: journalLimits(DefaultLimits()), Time: time.Now().UTC(),
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

func TestReconcileRepairsMissingParentTerminalFromChild(t *testing.T) {
	t.Parallel()

	repository, parent, workspaceID := newJournalFixture(t)
	child, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: workspaceID, Kind: session.KindSubagent,
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
		WorkspaceID: workspaceID, Kind: session.KindSubagent,
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
	parent, err := repository.Create(t.Context(), session.CreateOptions{WorkspaceID: workspaceID})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, parent.Close()) })

	return repository, parent, workspaceID
}
