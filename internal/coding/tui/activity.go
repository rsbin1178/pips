package tui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
)

type activityKind uint8

const (
	activityUnknown activityKind = iota
	activityPreparing
	activityWorking
	activityThinking
	activityResponding
	activityTool
	activityApproval
	activityRecovery
	activityCompacting
	activityPaused
	activityInterrupting
)

const (
	activityLabelPreparing    = "Preparing…"
	activityLabelWorking      = "Working…"
	activityLabelThinking     = "Thinking…"
	activityLabelResponding   = "Responding…"
	activityLabelTools        = "Running tools…"
	activityLabelApproval     = "Waiting for approval…"
	activityLabelRecovery     = "Waiting for recovery…"
	activityLabelCompacting   = "Compacting context…"
	activityLabelPaused       = "Paused…"
	activityLabelInterrupting = "Interrupting…"
)

type activityStatus struct {
	kind   activityKind
	label  string
	detail string
}

type activityContext struct {
	state       coding.State
	isStarting  bool
	hasBridge   bool
	isCanceling bool
}

var activitySpinner = spinner.Spinner{
	Frames: []string{"✻", "✢", "✳", "✢"},
	FPS:    120 * time.Millisecond,
}

type activityTickMsg = spinner.TickMsg

// activityIndicator owns only animation state. Runtime activity semantics are
// projected separately by resolveActivity so future states remain testable.
type activityIndicator struct {
	spinner spinner.Model
}

func newActivityIndicator() activityIndicator {
	return activityIndicator{
		spinner: spinner.New(spinner.WithSpinner(activitySpinner)),
	}
}

func (i *activityIndicator) Reset() {
	*i = newActivityIndicator()
}

func (i activityIndicator) Tick() tea.Cmd {
	return i.spinner.Tick
}

func (i *activityIndicator) Update(message activityTickMsg) tea.Cmd {
	var command tea.Cmd

	i.spinner, command = i.spinner.Update(message)

	return command
}

func (i activityIndicator) View(status activityStatus, theme colorTheme, noColor bool) string {
	frame := i.spinner.View()

	parts := []string{frame, status.label}
	if status.detail != "" {
		parts = append(parts, "·", status.detail)
	}

	if noColor {
		return strings.Join(parts, " ")
	}

	palette := paletteFor(theme)
	tone := activityColor(status.kind, palette)
	parts[0] = lipgloss.NewStyle().Bold(true).Foreground(tone).Render(parts[0])
	parts[1] = lipgloss.NewStyle().Bold(true).Foreground(tone).Render(parts[1])

	for index := 2; index < len(parts); index++ {
		parts[index] = lipgloss.NewStyle().Foreground(palette.muted).Render(parts[index])
	}

	return strings.Join(parts, " ")
}

func resolveActivity(context activityContext) (activityStatus, bool) {
	if status, visible := resolveBlockingActivity(context); visible {
		return status, true
	}

	if status, visible := resolveProgressActivity(context); visible {
		return status, true
	}

	return resolvePhaseActivity(context.state.Phase)
}

func resolveBlockingActivity(context activityContext) (activityStatus, bool) {
	if context.isCanceling {
		return activityStatus{
			kind: activityInterrupting, label: activityLabelInterrupting,
		}, true
	}

	switch context.state.Approval.Kind {
	case coding.ApprovalReview:
		return activityStatus{
			kind: activityApproval, label: activityLabelApproval,
		}, true
	case coding.ApprovalUncertain:
		return activityStatus{
			kind: activityRecovery, label: activityLabelRecovery,
		}, true
	case coding.ApprovalNone:
	}

	if context.state.Compaction.Active {
		return activityStatus{
			kind: activityCompacting, label: activityLabelCompacting,
		}, true
	}

	return activityStatus{}, false
}

func resolveProgressActivity(context activityContext) (activityStatus, bool) {
	if name, count := runningTools(context.state.Tools); count > 0 {
		if count == 1 {
			return activityStatus{
				kind: activityTool, label: fmt.Sprintf("Running %s…", name),
			}, true
		}

		return activityStatus{
			kind: activityTool, label: activityLabelTools,
			detail: fmt.Sprintf("%d active", count),
		}, true
	}

	if hasDraftKind(context.state.Draft, ai.StreamTextDelta) {
		return activityStatus{
			kind: activityResponding, label: activityLabelResponding,
		}, true
	}

	if hasDraftKind(context.state.Draft, ai.StreamReasoningDelta) ||
		hasOpenTurn(context.state.Runs) {
		return activityStatus{
			kind: activityThinking, label: activityLabelThinking,
		}, true
	}

	if context.isStarting || context.hasBridge {
		return activityStatus{
			kind: activityPreparing, label: activityLabelPreparing,
		}, true
	}

	return activityStatus{}, false
}

func resolvePhaseActivity(phase coding.Phase) (activityStatus, bool) {
	switch phase {
	case coding.PhaseRunning:
		return activityStatus{kind: activityWorking, label: activityLabelWorking}, true
	case coding.PhasePaused:
		return activityStatus{kind: activityPaused, label: activityLabelPaused}, true
	case coding.PhaseIdle, coding.PhaseClosing, coding.PhaseClosed, "":
		return activityStatus{}, false
	default:
		return activityStatus{}, false
	}
}

func runningTools(tools []coding.ToolState) (string, int) {
	name := ""
	count := 0

	for _, tool := range tools {
		if tool.Status != coding.ToolStatusRunning {
			continue
		}

		name = tool.Call.Name
		count++
	}

	return name, count
}

func hasDraftKind(deltas []coding.MessageDelta, kind ai.StreamEventType) bool {
	for _, delta := range deltas {
		if delta.Kind == kind {
			return true
		}
	}

	return false
}

func hasOpenTurn(runs []coding.RunState) bool {
	for _, run := range runs {
		if run.Active && run.TurnOpen {
			return true
		}
	}

	return false
}

func activityColor(kind activityKind, palette colorPalette) color.Color {
	switch kind {
	case activityPreparing, activityWorking:
		return palette.active
	case activityThinking, activityCompacting:
		return palette.model
	case activityResponding:
		return palette.session
	case activityTool:
		return palette.idle
	case activityApproval, activityRecovery, activityPaused, activityInterrupting:
		return palette.warning
	case activityUnknown:
		return palette.muted
	default:
		return palette.muted
	}
}
