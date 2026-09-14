package planmode_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rsbin1178/pips/internal/coding/planmode"
)

func TestStateTransitions(t *testing.T) {
	t.Parallel()

	// Inactive --> Pending (toggle on) --> Active (first prompt).
	state, err := planmode.StateInactive.ToggleOn()
	require.NoError(t, err)
	assert.Equal(t, planmode.StatePending, state)

	state, err = state.Activate()
	require.NoError(t, err)
	assert.Equal(t, planmode.StateActive, state)

	// Active --> Inactive (toggle off while idle).
	state, err = state.ToggleOff(false)
	require.NoError(t, err)
	assert.Equal(t, planmode.StateInactive, state)

	// Active --> ExitPending (toggle off mid-turn) --> Inactive (turn completes).
	state = planmode.StateActive
	state, err = state.ToggleOff(true)
	require.NoError(t, err)
	assert.Equal(t, planmode.StateExitPending, state)

	state, err = state.TurnComplete()
	require.NoError(t, err)
	assert.Equal(t, planmode.StateInactive, state)

	// Inactive --> Active (approved enter skips Pending).
	state, err = planmode.StateInactive.EnterApproved()
	require.NoError(t, err)
	assert.Equal(t, planmode.StateActive, state)

	// Active --> Inactive (approved exit, immediately).
	state, err = state.ExitApproved()
	require.NoError(t, err)
	assert.Equal(t, planmode.StateInactive, state)
}

func TestStateDurableCollapsesTransientStates(t *testing.T) {
	t.Parallel()

	assert.Equal(t, planmode.StateActive, planmode.StateActive.Durable())
	assert.Equal(t, planmode.StateInactive, planmode.StatePending.Durable())
	assert.Equal(t, planmode.StateInactive, planmode.StateExitPending.Durable())
	assert.Equal(t, planmode.StateInactive, planmode.StateInactive.Durable())
}

func TestStateGateAndPlanFlags(t *testing.T) {
	t.Parallel()

	assert.False(t, planmode.StateInactive.Plan())
	assert.False(t, planmode.StateInactive.GateArmed())
	assert.True(t, planmode.StatePending.Plan())
	assert.False(t, planmode.StatePending.GateArmed())
	assert.True(t, planmode.StateActive.Plan())
	assert.True(t, planmode.StateActive.GateArmed())
	assert.True(t, planmode.StateExitPending.Plan())
	assert.True(t, planmode.StateExitPending.GateArmed())
}

func TestStateRejectsUnknownValues(t *testing.T) {
	t.Parallel()

	var state planmode.State = "exploded"
	assert.False(t, state.Valid())
	_, err := state.ToggleOn()
	require.ErrorIs(t, err, planmode.ErrInvalid)
	_, err = state.ToggleOff(false)
	require.ErrorIs(t, err, planmode.ErrInvalid)
	_, err = state.Activate()
	require.ErrorIs(t, err, planmode.ErrInvalid)
	_, err = state.EnterApproved()
	require.ErrorIs(t, err, planmode.ErrInvalid)
	_, err = state.ExitApproved()
	require.ErrorIs(t, err, planmode.ErrInvalid)
	_, err = state.TurnComplete()
	require.ErrorIs(t, err, planmode.ErrInvalid)
}

func TestStoreRoundTripAndSeed(t *testing.T) {
	t.Parallel()

	sessions := t.TempDir()
	store, err := planmode.NewStore(sessions, "session-1", planmode.DefaultLimits())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(sessions, "session-1", "plan.md"), store.Path())

	// Missing plan is NotFound until seeded.
	_, err = store.Read(t.Context())
	require.ErrorIs(t, err, planmode.ErrNotFound)

	created, err := store.Seed(t.Context())
	require.NoError(t, err)
	assert.True(t, created)

	document, err := store.Read(t.Context())
	require.NoError(t, err)
	assert.Empty(t, document.Content)

	// Seeding again never truncates existing content.
	_, err = store.Replace(t.Context(), "# Plan\n\nKeep me.\n")
	require.NoError(t, err)

	created, err = store.Seed(t.Context())
	require.NoError(t, err)
	assert.False(t, created)

	document, err = store.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "# Plan\n\nKeep me.\n", document.Content)

	info, err := os.Lstat(store.Path())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStoreRejectsInvalidContentAndOversize(t *testing.T) {
	t.Parallel()

	store, err := planmode.NewStore(t.TempDir(), "session-2", planmode.Limits{MaxBytes: 32})
	require.NoError(t, err)

	_, err = store.Replace(t.Context(), strings.Repeat("a", 33))
	require.ErrorIs(t, err, planmode.ErrInvalid)

	_, err = store.Replace(t.Context(), "bad\x00content")
	require.ErrorIs(t, err, planmode.ErrInvalid)

	_, err = store.Replace(t.Context(), "# ok")
	require.NoError(t, err)
}

func TestStoreRejectsSymlinkedPlanFile(t *testing.T) {
	t.Parallel()

	sessions := t.TempDir()
	store, err := planmode.NewStore(sessions, "session-3", planmode.DefaultLimits())
	require.NoError(t, err)

	victim := filepath.Join(sessions, "victim.md")
	require.NoError(t, os.WriteFile(victim, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(victim, store.Path()))

	_, err = store.Read(t.Context())
	require.ErrorIs(t, err, planmode.ErrUnsafe)

	_, err = store.Replace(t.Context(), "# plan")
	require.ErrorIs(t, err, planmode.ErrUnsafe)
}

func TestStoreStatePersistenceCollapsesTransientStates(t *testing.T) {
	t.Parallel()

	sessions := t.TempDir()
	store, err := planmode.NewStore(sessions, "session-4", planmode.DefaultLimits())
	require.NoError(t, err)

	state, err := store.LoadState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, planmode.StateInactive, state)

	require.NoError(t, store.SaveState(t.Context(), planmode.StatePending))

	// A fresh store sees the collapsed durable projection.
	reopened, err := planmode.NewStore(sessions, "session-4", planmode.DefaultLimits())
	require.NoError(t, err)

	state, err = reopened.LoadState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, planmode.StateInactive, state)

	require.NoError(t, store.SaveState(t.Context(), planmode.StateActive))

	state, err = reopened.LoadState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, planmode.StateActive, state)
}

func TestStoreCopyToCarriesPlanAndDurableState(t *testing.T) {
	t.Parallel()

	sessions := t.TempDir()
	source, err := planmode.NewStore(sessions, "session-5", planmode.DefaultLimits())
	require.NoError(t, err)
	target, err := planmode.NewStore(sessions, "session-6", planmode.DefaultLimits())
	require.NoError(t, err)

	// No plan is a successful no-op.
	require.NoError(t, source.CopyTo(t.Context(), target))

	_, err = source.Replace(t.Context(), "# Plan\n")
	require.NoError(t, err)
	require.NoError(t, source.SaveState(t.Context(), planmode.StateActive))
	require.NoError(t, source.CopyTo(t.Context(), target))

	document, err := target.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "# Plan\n", document.Content)

	state, err := target.LoadState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, planmode.StateActive, state)
}

func TestStoreRequiresValidSessionIdentity(t *testing.T) {
	t.Parallel()

	_, err := planmode.NewStore(t.TempDir(), "bad/id", planmode.DefaultLimits())
	require.ErrorIs(t, err, planmode.ErrInvalid)
}

func TestPromptTextsAreStable(t *testing.T) {
	t.Parallel()

	names := planmode.ReminderNames{Edit: "apply_patch", Ask: "ask_user", Exit: planmode.ExitToolName}

	reminder := planmode.PlanReminder("/sessions/s1/plan.md", false, names)
	assert.Contains(t, reminder, "Plan mode is active. Do not make any edits or writes to the system.")
	assert.Contains(t, reminder, "No plan written yet. Write your plan to /sessions/s1/plan.md using the apply_patch tool.")
	assert.Contains(t, reminder, "Note that this is the only file you are allowed to edit.")
	assert.Contains(t, reminder, "Your turn should only end with either ask_user to clarify requirements or exit_plan_mode to present your plan to the user.")

	withContent := planmode.PlanReminder("/sessions/s1/plan.md", true, names)
	assert.Contains(t, withContent, "A plan file exists at /sessions/s1/plan.md.")

	rejection := planmode.EditRejection("/sessions/s1/plan.md")
	assert.Equal(t,
		"Rejected: file edits are not allowed in plan mode - the only editable file is the plan file (/sessions/s1/plan.md).",
		rejection,
	)

	assert.Equal(t, planmode.ExitApprovedResult, planmode.ApprovalResult(nil))
	assert.Contains(t, planmode.ApprovalResult([]string{"fix the casing"}), "The user approved the plan with the following review comments:\nfix the casing")
	assert.Equal(t, planmode.ExitReviseResult, planmode.RevisionResult(""))
	assert.Contains(t, planmode.RevisionResult("use Redis"), "User revision notes:\nuse Redis")
}

func TestStoreContextCancellation(t *testing.T) {
	t.Parallel()

	store, err := planmode.NewStore(t.TempDir(), "session-7", planmode.DefaultLimits())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = store.Read(ctx)
	require.True(t, errors.Is(err, context.Canceled))
}
