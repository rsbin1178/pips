package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
)

const (
	goalActionStart  = "start"
	goalActionClear  = "clear"
	goalActionPause  = "pause"
	goalActionResume = "resume"
)

type goalController interface {
	StartGoal(context.Context, coding.GoalRequest) iter.Seq2[coding.Event, error]
	ResumeGoal(context.Context) iter.Seq2[coding.Event, error]
	PauseGoal(context.Context) error
	ClearGoal(context.Context) error
}

type goalCommand struct {
	action  string
	request coding.GoalRequest
}

type goalControlResultMsg struct {
	generation uint64
	sessionID  string
	state      coding.State
	err        error
}

func parseGoalCommand(arguments string) (goalCommand, error) {
	value := strings.TrimSpace(arguments)
	if value == "" {
		return goalCommand{action: commandStatus}, nil
	}

	fields := strings.Fields(value)
	switch fields[0] {
	case commandStatus, goalActionPause, goalActionResume, goalActionClear:
		if len(fields) != 1 {
			return goalCommand{}, errors.New("goal controls do not accept arguments")
		}

		return goalCommand{action: fields[0]}, nil
	}

	request := coding.GoalRequest{Condition: value}

	for index, field := range fields {
		if field != "--budget" && !strings.HasPrefix(field, "--budget=") {
			continue
		}

		budget := strings.TrimPrefix(field, "--budget=")
		if field == "--budget" {
			if index != len(fields)-2 {
				return goalCommand{}, errors.New("use /goal <condition> --budget <positive tokens>")
			}

			budget = fields[index+1]
		} else if index != len(fields)-1 {
			return goalCommand{}, errors.New("goal budget must follow the condition")
		}

		amount, err := strconv.Atoi(budget)
		if err != nil || amount <= 0 {
			return goalCommand{}, errors.New("goal budget must be a positive integer")
		}

		request.Condition = strings.TrimSpace(value[:strings.LastIndex(value, field)])
		request.MaxTokens = amount

		break
	}

	if err := request.Validate(); err != nil {
		return goalCommand{}, err
	}

	return goalCommand{action: goalActionStart, request: request}, nil
}

func (m *Model) executeGoalCommand(arguments string) (tea.Model, tea.Cmd) {
	command, err := parseGoalCommand(arguments)
	if err != nil {
		m.picker.err = err

		return m, nil
	}

	if command.action == commandStatus {
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)

		return m, m.printInspection("Goal", goalStatusText(m.state.Goal))
	}

	controller, ok := m.controller.(goalController)
	if !ok {
		m.picker.err = errors.New("this controller does not support goals")

		return m, nil
	}

	if command.action == goalActionPause || command.action == goalActionClear {
		return m, m.runGoalControl(controller, command.action)
	}

	actionContext := m.actionContext()
	if actionContext != contextIdle && (command.action != goalActionResume || actionContext != contextPaused) {
		m.picker.err = errors.New("start or resume a goal only while idle")

		return m, nil
	}

	previous := m.picker.previousComposer
	m.closeCommandPicker(false)
	m.restoreCommandComposer(previous)

	return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		if command.action == goalActionResume {
			return controller.ResumeGoal(ctx)
		}

		return controller.StartGoal(ctx, command.request)
	})
}

func (m *Model) runGoalControl(controller goalController, action string) tea.Cmd {
	m.pickerSeq++
	m.picker.generation = m.pickerSeq
	m.picker.loading = true
	m.picker.controlling = true
	m.picker.err = nil
	generation, sessionID := m.picker.generation, m.state.SessionID
	ctx, owner := m.ctx, m.controller

	return func() tea.Msg {
		var err error
		if action == goalActionClear {
			err = controller.ClearGoal(ctx)
		} else {
			err = controller.PauseGoal(ctx)
		}

		return goalControlResultMsg{
			generation: generation, sessionID: sessionID, state: owner.Snapshot(), err: err,
		}
	}
}

func (m *Model) applyGoalControl(message goalControlResultMsg) (tea.Model, tea.Cmd) {
	if message.sessionID != m.state.SessionID || m.picker.kind != pickerCommand ||
		message.generation != m.picker.generation {
		return m, nil
	}

	m.picker.loading = false

	m.picker.controlling = false
	if message.err != nil {
		m.picker.err = message.err

		return m, nil
	}

	if message.state.SessionID == m.state.SessionID && message.state.Sequence >= m.state.Sequence {
		m.state = message.state
	}

	previous := m.picker.previousComposer
	m.closeCommandPicker(false)
	m.restoreCommandComposer(previous)

	return m, m.printInspection("Goal", goalStatusText(m.state.Goal))
}

func (m *Model) goalCommandPickerActive() bool {
	return m.state.Goal.ID != "" && m.picker.kind == pickerCommand
}

func goalStatusText(state coding.GoalState) string {
	if state.ID == "" {
		return "No goal set. Use /goal <condition> [--budget <tokens>]."
	}

	budget := "unset"
	if state.MaxTokens > 0 {
		budget = strconv.Itoa(state.MaxTokens)
	}

	lines := []string{
		"Condition: " + state.Condition,
		"Status: " + string(state.Status),
		fmt.Sprintf("Work segments: %d · Evaluations: %d", state.Attempts, state.Evaluations),
		fmt.Sprintf("Tokens: %d · Budget: %s", state.Tokens, budget),
	}
	if state.Reason != "" {
		lines = append(lines, "Reason: "+state.Reason)
	}

	if len(state.Gaps) > 0 {
		lines = append(lines, "Verification gaps:")
		for _, gap := range state.Gaps {
			lines = append(lines, "- "+gap)
		}
	}

	if state.FailedCheckTokens > 0 {
		lines = append(lines, fmt.Sprintf("Observed failed-check tokens: %d (included above)", state.FailedCheckTokens))
	}

	return strings.Join(lines, "\n")
}
