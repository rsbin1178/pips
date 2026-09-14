//nolint:wsl_v5 // Plan-mode activation assertions stay beside their events.
package coding

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
)

func TestRuntimePendingPlanModeActivatesOnFirstPrompt(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)

	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))

	pending := runtime.Snapshot()
	require.Equal(t, planmode.StatePending, pending.PlanMode)
	require.Equal(t, ModePlan, pending.Mode)

	setRuntimeResponses(model, runtimeTextResponse("planning"))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan this change")))

	assert.Equal(t,
		[]PlanModeChanged{{State: planmode.StateActive}},
		payloadsOfType[PlanModeChanged](events, EventPlanModeChanged),
	)
	assert.Equal(t, planmode.StateActive, runtime.Snapshot().PlanMode)

	// The activation is durable: only the Active state survives a restart.
	state, err := runtime.planStore.LoadState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, planmode.StateActive, state)
}

func TestRuntimeExitPendingSettlesWithoutActivatingOnNotification(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)

	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))
	require.NoError(t, runtime.SetMode(t.Context(), ModeAgent))
	assert.Equal(t, planmode.StateInactive, runtime.Snapshot().PlanMode)

	setRuntimeResponses(model, runtimeTextResponse("planning"))
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan this change")))

	// Prompting after a full exit starts in agent mode and never re-activates.
	assert.Equal(t, planmode.StateInactive, runtime.Snapshot().PlanMode)
}
