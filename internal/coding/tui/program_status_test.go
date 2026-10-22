package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProgramStatusReportsLifecycleAndPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*Model)
		state   tea.ProgramState
		kind    tea.ProgramStatusKind
	}{
		{
			name:    "trust",
			prepare: func(model *Model) { model.lifecycle = lifecycleTrust },
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindPermission,
		},
		{
			name:    "loading",
			prepare: func(model *Model) { model.lifecycle = lifecycleLoading },
			state:   tea.ProgramStateWorking,
		},
		{
			name:    "idle",
			prepare: func(*Model) {},
			state:   tea.ProgramStateIdle,
		},
		{
			name:    "running",
			prepare: func(model *Model) { model.state.Phase = coding.PhaseRunning },
			state:   tea.ProgramStateWorking,
		},
		{
			name:    "paused",
			prepare: func(model *Model) { model.state.Phase = coding.PhasePaused },
			state:   tea.ProgramStateBlocked,
		},
		{
			name:    "closing",
			prepare: func(model *Model) { model.state.Phase = coding.PhaseClosing },
			state:   tea.ProgramStateDone,
		},
		{
			name:    "fatal",
			prepare: func(model *Model) { model.lifecycle = lifecycleFatal },
			state:   tea.ProgramStateError,
		},
		{
			name:    "approval",
			prepare: func(model *Model) { model.prompt.kind = promptApproval },
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindPermission,
		},
		{
			name:    "question",
			prepare: func(model *Model) { model.prompt.kind = promptQuestion },
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindQuestion,
		},
		{
			name:    "plan review",
			prepare: func(model *Model) { model.prompt.kind = promptPlanReview },
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindPermission,
		},
		{
			name:    "compaction choice",
			prepare: func(model *Model) { model.prompt.kind = promptCompact },
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindQuestion,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := readyModel(t, true)
			test.prepare(model)

			report := model.programStatus()
			require.NotNil(t, report)
			assert.Equal(t, test.state, report.State)
			assert.Equal(t, test.kind, report.Kind)
			assert.Equal(t, "pips", report.App)
		})
	}
}

func TestProgramStatusReusesReportUntilFieldsChange(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)

	first := model.View().ProgramStatus
	require.NotNil(t, first)
	assert.Same(t, first, model.View().ProgramStatus)

	model.state.Phase = coding.PhaseRunning
	second := model.View().ProgramStatus
	require.NotNil(t, second)
	assert.NotSame(t, first, second)
	assert.Equal(t, tea.ProgramStateIdle, first.State)
	assert.Equal(t, tea.ProgramStateWorking, second.State)
}
