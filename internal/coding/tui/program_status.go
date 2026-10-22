package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
)

const programStatusTitle = "Pips"

// programStatusFields is the comparable identity of one OSC 7501 report.
type programStatusFields struct {
	state   tea.ProgramState
	kind    tea.ProgramStatusKind
	title   string
	message string
}

// programStatus returns the report for the current lifecycle, prompt and run
// phase. The report is coarse on purpose: Bubble Tea writes one short OSC 7501
// sequence with the next frame when the fields change, and nothing while they
// hold, so per-token progress must never enter here. Reusing the pointer for an
// unchanged state keeps the renderer's comparison allocation-free.
func (m *Model) programStatus() *tea.ProgramStatus {
	fields := m.currentProgramStatus()
	if m.statusReport != nil && fields == m.statusFields {
		return m.statusReport
	}

	m.statusFields = fields
	m.statusReport = &tea.ProgramStatus{
		State:   fields.state,
		App:     "pips",
		Kind:    fields.kind,
		Title:   fields.title,
		Message: fields.message,
	}

	return m.statusReport
}

func (m *Model) currentProgramStatus() programStatusFields {
	switch m.lifecycle {
	case lifecycleFatal:
		return programStatusFields{
			state:   tea.ProgramStateError,
			title:   programStatusTitle,
			message: "Startup failed",
		}
	case lifecycleTrust:
		return programStatusFields{
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindPermission,
			title:   programStatusTitle,
			message: "Waiting for workspace trust",
		}
	case lifecycleLoading:
		return programStatusFields{
			state:   tea.ProgramStateWorking,
			title:   programStatusTitle,
			message: "Starting",
		}
	case lifecycleReady:
		// The prompt and run phase below own the ready state.
	}

	if fields, ok := m.promptProgramStatus(); ok {
		return fields
	}

	return m.phaseProgramStatus()
}

func (m *Model) promptProgramStatus() (programStatusFields, bool) {
	switch m.prompt.kind {
	case promptApproval:
		return programStatusFields{
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindPermission,
			title:   programStatusTitle,
			message: "Waiting for approval",
		}, true
	case promptQuestion:
		return programStatusFields{
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindQuestion,
			title:   programStatusTitle,
			message: "Waiting for your answer",
		}, true
	case promptPlanReview:
		return programStatusFields{
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindPermission,
			title:   programStatusTitle,
			message: "Waiting for plan review",
		}, true
	case promptCompact:
		return programStatusFields{
			state:   tea.ProgramStateBlocked,
			kind:    tea.ProgramStatusKindQuestion,
			title:   programStatusTitle,
			message: "Waiting for compaction choice",
		}, true
	case promptNone, promptPlanView:
		// Nothing is blocking; the run phase decides.
	}

	return programStatusFields{}, false
}

func (m *Model) phaseProgramStatus() programStatusFields {
	switch m.state.Phase {
	case coding.PhaseIdle:
		// A turn that is starting but has not advanced the phase is still working.
	case coding.PhaseClosing, coding.PhaseClosed:
		return programStatusFields{
			state:   tea.ProgramStateDone,
			title:   programStatusTitle,
			message: "Done",
		}
	case coding.PhasePaused:
		return programStatusFields{
			state:   tea.ProgramStateBlocked,
			title:   programStatusTitle,
			message: "Paused",
		}
	case coding.PhaseRunning:
		return programStatusFields{
			state:   tea.ProgramStateWorking,
			title:   programStatusTitle,
			message: "Working",
		}
	}

	if m.starting || m.bridge != nil {
		return programStatusFields{
			state:   tea.ProgramStateWorking,
			title:   programStatusTitle,
			message: "Working",
		}
	}

	return programStatusFields{
		state:   tea.ProgramStateIdle,
		title:   programStatusTitle,
		message: "Idle",
	}
}
