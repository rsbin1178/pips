package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding"
)

const (
	teamRouteWideWidth        = 72
	teamRouteObjectiveBytes   = 512
	teamRoutePrimaryTextBytes = 160
	teamRouteDetailTextBytes  = 256
	unknownTeamRouteActivity  = "unknown"
	teamRouteOuterPadding     = 2
)

type teamRouteTone uint8

const (
	teamRouteTonePrimary teamRouteTone = iota
	teamRouteToneAccent
	teamRouteToneMuted
	teamRouteToneActive
	teamRouteToneSuccess
	teamRouteToneWarning
	teamRouteToneError
)

type teamRouteActivityView struct {
	title  string
	detail string
	hint   string
}

func teamRouteActivity(
	operation teamRouteOperation,
	cancelling bool,
) teamRouteActivityView {
	activity := teamRouteOperationActivity(operation)
	if cancelling {
		activity.title = "Cancelling " + activity.cancelNoun
		activity.detail = "Waiting for the current Team operation to stop safely."
		activity.hint = "Esc cancellation requested"
	}

	return teamRouteActivityView{
		title: activity.title, detail: activity.detail, hint: activity.hint,
	}
}

type teamRouteOperationActivityView struct {
	title      string
	cancelNoun string
	detail     string
	hint       string
}

var teamRouteOperationActivities = [...]teamRouteOperationActivityView{
	teamRouteOperationNone: fallbackTeamRouteActivity(),
	teamRouteOperationGenerate: {
		title: "Designing Team proposal", cancelNoun: "proposal generation",
		detail: "The constrained Lead is designing up to 3 Workers and a task DAG.",
		hint:   "Esc cancel generation",
	},
	teamRouteOperationRevise: {
		title: "Revising Team proposal", cancelNoun: "proposal revision",
		detail: "The constrained Lead is revising the reviewed Workers and task DAG.",
		hint:   "Esc cancel revision",
	},
	teamRouteOperationConfirm: {
		title: "Admitting Coding Team", cancelNoun: "Team admission",
		detail: "Creating the reviewed Team from the exact admission choice.",
		hint:   "Esc cancel admission",
	},
	teamRouteOperationRead: {
		title: "Refreshing Team state", cancelNoun: "Team refresh",
		detail: "Reading the latest Team, Worker, task, and control state.",
		hint:   "Esc cancel refresh",
	},
	teamRouteOperationControl: {
		title: "Applying Team control", cancelNoun: "Team control",
		detail: "Submitting the reviewed control to its exact Team target.",
		hint:   "Esc cancel control",
	},
	teamRouteOperationDecline: {
		title: "Discarding Team proposal", cancelNoun: "proposal discard",
		detail: "Removing the unadmitted proposal without starting Workers.",
		hint:   "Esc cancel discard",
	},
	teamRouteOperationDiscoverRecovery: {
		title: "Checking Team recovery", cancelNoun: "recovery check",
		detail: "Looking for retained Team resources without starting execution.",
		hint:   "Esc cancel recovery check",
	},
	teamRouteOperationResumeRecovery: {
		title: "Resuming Coding Team", cancelNoun: "Team resume",
		detail: "Applying the reviewed recovery choice to the retained Team.",
		hint:   "Esc cancel resume",
	},
	teamRouteOperationLoadIntegration: {
		title: "Loading Integration state", cancelNoun: "Integration load",
		detail: "Reading completed tasks and recoverable Integration work.",
		hint:   "Esc cancel Integration load",
	},
	teamRouteOperationPrepareIntegration: {
		title: "Preparing Integration preview", cancelNoun: "Integration preview",
		detail: "Preparing a reviewable preview from the selected completed tasks.",
		hint:   "Esc cancel preview",
	},
	teamRouteOperationApplyIntegration: {
		title: "Applying Integration", cancelNoun: "Integration apply",
		detail: "Applying the exact reviewed Integration preview.",
		hint:   "Esc cancel Integration apply",
	},
	teamRouteOperationRejectIntegration: {
		title: "Rejecting Integration preview", cancelNoun: "Integration rejection",
		detail: "Rejecting the exact reviewed Integration preview.",
		hint:   "Esc cancel Integration rejection",
	},
	teamRouteOperationRecoverIntegration: {
		title: "Recovering Integration", cancelNoun: "Integration recovery",
		detail: "Applying the selected manager-provided recovery action.",
		hint:   "Esc cancel Integration recovery",
	},
	teamRouteOperationCleanup: {
		title: "Cleaning Team resources", cancelNoun: "Team cleanup",
		detail: "Cleaning only exact eligible Team resources; drift is retained.",
		hint:   "Esc cancel cleanup",
	},
}

func teamRouteOperationActivity(operation teamRouteOperation) teamRouteOperationActivityView {
	if int(operation) >= len(teamRouteOperationActivities) {
		return fallbackTeamRouteActivity()
	}

	return teamRouteOperationActivities[operation]
}

func fallbackTeamRouteActivity() teamRouteOperationActivityView {
	return teamRouteOperationActivityView{
		title: "Team operation in progress", cancelNoun: "Team operation",
		detail: "Waiting for the current Team operation to complete.",
		hint:   "Esc cancel operation",
	}
}

func (m *Model) teamRouteStyle(value string, tone teamRouteTone, bold bool) string {
	if m.options.NoColor || value == "" {
		return value
	}

	palette := paletteFor(m.theme)
	color := palette.workspace

	switch tone {
	case teamRouteToneAccent:
		color = palette.session
	case teamRouteToneMuted:
		color = palette.muted
	case teamRouteToneActive:
		color = palette.active
	case teamRouteToneSuccess:
		color = palette.idle
	case teamRouteToneWarning:
		color = palette.warning
	case teamRouteToneError:
		color = palette.error
	case teamRouteTonePrimary:
	}

	return lipgloss.NewStyle().Bold(bold).Foreground(color).Render(value)
}

func (m *Model) teamRouteHeader(description string) []string {
	rail, retainedDescription := m.teamRouteRail(description)

	lines := []string{"", rail, ""}
	if description != "" && !retainedDescription {
		lines = m.appendTeamRouteWrapped(
			lines,
			boundedTeamRouteText(description, teamRouteObjectiveBytes),
			"    ",
			teamRouteToneMuted,
		)
		lines = append(lines, "")
	}

	return lines
}

func (m *Model) teamRouteRail(description string) (string, bool) {
	innerWidth := max(1, m.width-(teamRouteOuterPadding*2))
	prefix := "─ "
	title := "Coding Team"
	plain := prefix + title
	retainedDescription := false

	description = boundedTeamRouteText(description, teamRoutePrimaryTextBytes)
	if description != "" {
		candidate := plain + " ─ " + description
		if ansi.StringWidth(candidate) <= innerWidth-2 {
			plain = candidate
			retainedDescription = true
		}
	}

	if remaining := innerWidth - ansi.StringWidth(plain); remaining > 0 {
		plain += " " + strings.Repeat("─", max(0, remaining-1))
	}

	plain = ansi.Truncate(plain, innerWidth, "")

	if m.options.NoColor {
		return strings.Repeat(" ", teamRouteOuterPadding) + plain, retainedDescription
	}

	before, after, found := strings.Cut(plain, title)
	if !found {
		return strings.Repeat(" ", teamRouteOuterPadding) +
			m.teamRouteStyle(plain, teamRouteToneAccent, false), retainedDescription
	}

	styled := m.teamRouteStyle(before, teamRouteToneAccent, false) +
		m.teamRouteStyle(title, teamRouteToneAccent, true) +
		m.teamRouteStyle(after, teamRouteToneAccent, false)

	return strings.Repeat(" ", teamRouteOuterPadding) + styled, retainedDescription
}

func (m *Model) appendTeamRouteWrapped(
	lines []string,
	value string,
	indent string,
	tone teamRouteTone,
) []string {
	width := max(1, m.width-ansi.StringWidth(indent)-teamRouteOuterPadding)

	wrapped := lipgloss.Wrap(value, width, "")
	for line := range strings.SplitSeq(wrapped, "\n") {
		lines = append(lines, m.teamRouteStyle(indent+line, tone, false))
	}

	return lines
}

func (m *Model) teamRouteSection(label string) string {
	return m.teamRouteStyle("  "+label, teamRouteTonePrimary, true)
}

func (m *Model) teamRouteMetadata(value string) string {
	return m.teamRouteStyle("    └ "+value, teamRouteToneMuted, false)
}

func (m *Model) teamRouteFooter(value string) string {
	return m.teamRouteStyle("  "+value, teamRouteToneMuted, false)
}

func (m *Model) teamRouteEntityRow(
	marker string,
	ordinal string,
	title string,
	status string,
) string {
	glyph, tone := teamRouteStateGlyph(status)

	prefix := marker + glyph + " " + ordinal + "  "
	if marker == "" {
		prefix = "  " + glyph + " " + ordinal + "  "
	}

	return m.teamRouteStyle(prefix, tone, true) +
		m.teamRouteStyle(title, teamRouteTonePrimary, true)
}

func teamRouteStateGlyph(status string) (string, teamRouteTone) {
	switch status {
	case string(team.TaskStatusRunning), string(team.TaskStatusClaimed):
		return "✻", teamRouteToneActive
	case string(team.TaskStatusCompleted), "applied", "complete":
		return "✓", teamRouteToneSuccess
	case string(team.TaskStatusFailed), "rejected":
		return "✗", teamRouteToneError
	case string(team.TaskStatusCancelled), string(team.MemberStatusDisabled):
		return "⊘", teamRouteToneWarning
	case string(team.MemberStatusActive):
		return "●", teamRouteToneSuccess
	default:
		return "○", teamRouteToneMuted
	}
}

func (m *Model) teamRouteActivityLine(activity teamRouteActivityView, cancelling bool) string {
	kind := activityWorking
	if cancelling {
		kind = activityInterrupting
	}

	return "  " + m.activity.View(activityStatus{
		kind: kind, label: activity.title,
	}, m.theme, m.options.NoColor)
}

func (m *Model) teamRouteLoadingContent(content string, state *teamRouteState) string {
	activity := teamRouteActivity(state.pending, state.cancelRequested)

	if state.pending == teamRouteOperationGenerate || state.pending == teamRouteOperationRevise {
		lines := m.teamRouteHeader(state.objective)
		lines = append(lines,
			m.teamRouteActivityLine(activity, state.cancelRequested),
			m.teamRouteStyle("    "+activity.detail, teamRouteToneMuted, false),
			"",
			m.teamRouteFooter(activity.hint),
			"",
		)

		return strings.Join(lines, "\n")
	}

	return strings.Join([]string{
		content,
		"",
		m.teamRouteActivityLine(activity, state.cancelRequested),
		m.teamRouteStyle("    "+activity.detail, teamRouteToneMuted, false),
		m.teamRouteFooter(activity.hint),
	}, "\n")
}

func (m *Model) teamProposalLines() []string {
	proposal := m.route.team.proposal
	if proposal == nil {
		return []string{"Team proposal unavailable."}
	}

	workspace := "clean Workspace"
	if proposal.Dirty {
		workspace = "dirty Workspace · uncommitted changes are excluded from admission"
	}

	lines := m.teamRouteHeader("Review proposal")
	lines = m.appendTeamRouteWrapped(
		lines,
		boundedTeamRouteText(proposal.Request.Objective, teamRouteObjectiveBytes),
		"    ",
		teamRouteToneMuted,
	)
	lines = append(lines,
		"",
		m.teamRouteStyle("  Admission · "+workspace, teamRouteToneMuted, false),
		m.teamRouteStyle(
			"  Expires · "+proposal.ExpiresAt.Local().Format(time.DateTime),
			teamRouteToneMuted,
			false,
		),
		"",
		m.teamRouteSection(fmt.Sprintf("Workers · %d", len(proposal.Request.Workers))),
	)

	for index, worker := range proposal.Request.Workers {
		name := boundedTeamRouteText(worker.Name, teamRoutePrimaryTextBytes)
		role := boundedTeamRouteText(worker.Role, teamRouteDetailTextBytes)
		lines = append(lines,
			m.teamRouteEntityRow("", fmt.Sprintf("W%d", index+1), name, "pending"),
			m.teamRouteMetadata(role),
		)
	}

	lines = append(lines, "", m.teamRouteSection(fmt.Sprintf(
		"Task DAG · %d",
		len(proposal.Request.Tasks),
	)))
	ordinals := teamProposalTaskOrdinals(proposal.Request.Tasks)

	for index, taskValue := range proposal.Request.Tasks {
		title := boundedTeamRouteText(taskValue.Title, teamRoutePrimaryTextBytes)
		worker := boundedTeamRouteText(taskValue.AssignedWorker, teamRoutePrimaryTextBytes)
		dependency := teamProposalDependencyLabel(taskValue.Dependencies, ordinals)

		lines = append(lines, m.teamRouteEntityRow(
			"",
			fmt.Sprintf("T%d", index+1),
			title,
			"ready",
		))
		if m.width >= teamRouteWideWidth {
			lines = append(lines, m.teamRouteMetadata(worker+" · "+dependency))
		} else {
			lines = append(lines, m.teamRouteMetadata(worker), m.teamRouteMetadata(dependency))
		}

		if description := boundedTeamRouteText(taskValue.Description, teamRouteDetailTextBytes); description != "" {
			lines = append(lines, m.teamRouteMetadata(description))
		}
	}

	return lines
}

func teamProposalTaskOrdinals(tasks []coding.TeamTaskSpec) map[string]int {
	ordinals := make(map[string]int, len(tasks))
	for index, taskValue := range tasks {
		ordinals[taskValue.ID] = index + 1
	}

	return ordinals
}

func teamProposalDependencyLabel(dependencies []string, ordinals map[string]int) string {
	if len(dependencies) == 0 {
		return "ready"
	}

	labels := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		if ordinal, ok := ordinals[dependency]; ok {
			labels = append(labels, fmt.Sprintf("T%d", ordinal))
		} else {
			labels = append(labels, "unknown task")
		}
	}

	return "after " + strings.Join(labels, ", ")
}

func (m *Model) activeTeamContent() string {
	state := m.route.team
	if state.view == nil {
		lines := m.teamRouteHeader("Loading")
		activity := teamRouteActivity(teamRouteOperationRead, false)
		lines = append(lines,
			m.teamRouteActivityLine(activity, false),
			m.teamRouteStyle("    Reading the latest Team state.", teamRouteToneMuted, false),
			"",
			m.teamRouteFooter("r retry · Esc close"),
		)

		return strings.Join(lines, "\n")
	}

	view := state.view
	lines := m.teamRouteHeader(boundedTeamRouteText(
		string(view.Status),
		teamRoutePrimaryTextBytes,
	))
	lines = m.appendTeamRouteWrapped(
		lines,
		boundedTeamRouteText(view.Objective, teamRouteObjectiveBytes),
		"    ",
		teamRouteToneMuted,
	)
	lines = append(lines,
		"",
		m.teamRouteSection(fmt.Sprintf(
			"Workers · %d · Tasks · %d%s",
			len(view.Members),
			len(view.Tasks),
			teamTaskStatusSummary(view.Tasks),
		)),
		m.teamRouteStyle(
			"  Resources · "+boundedTeamRouteText(
				string(view.ResourceState),
				teamRoutePrimaryTextBytes,
			),
			teamRouteToneMuted,
			false,
		),
		"",
		m.teamRouteSection(fmt.Sprintf("Workers · %d", len(view.Members))),
	)

	for index, member := range view.Members {
		name := boundedTeamRouteText(member.Name, teamRoutePrimaryTextBytes)
		role := boundedTeamRouteText(member.Role, teamRouteDetailTextBytes)
		lines = append(lines,
			m.teamRouteEntityRow(
				"",
				fmt.Sprintf("W%d", index+1),
				name,
				string(member.Status),
			),
			m.teamRouteMetadata(fmt.Sprintf("%s · %s", role, member.Status)),
		)
	}

	lines = append(lines, "", m.teamRouteSection(fmt.Sprintf("Tasks · %d", len(view.Tasks))))
	for index, taskValue := range view.Tasks {
		marker := ""
		if index == m.route.cursor {
			marker = "› "
		}

		title := boundedTeamRouteText(taskValue.Title, teamRoutePrimaryTextBytes)
		worker := boundedTeamRouteText(
			teamRouteMemberName(view, taskValue.AssignedMemberID),
			teamRoutePrimaryTextBytes,
		)

		metadata := fmt.Sprintf("%s · %s", worker, taskValue.Status)
		if attempt, found := latestTeamRouteAttempt(view, taskValue.ID); found {
			metadata += " · " + teamAttemptSummary(attempt)
		}

		lines = append(lines,
			m.teamRouteEntityRow(
				marker,
				fmt.Sprintf("T%d", index+1),
				title,
				string(taskValue.Status),
			),
			m.teamRouteMetadata(metadata),
		)
	}

	lines = appendTeamRouteControls(lines, view.Controls)
	lines = append(lines, "")

	if m.controller.Mode().Current == coding.ModePlan {
		lines = append(lines, m.teamRouteFooter(
			"Plan Mode · Team controls are read-only · ↑/↓ choose · Esc close",
		))
	} else {
		lines = append(lines,
			m.teamRouteFooter(
				"↑/↓ choose · g integrate/recover · m message · f follow-up · i interrupt · x cancel task · r retry · C cancel Team · Esc close",
			),
		)
	}

	return strings.Join(lines, "\n")
}

func teamTaskStatusSummary(tasks []coding.TeamTaskView) string {
	order := []team.TaskStatus{
		team.TaskStatusRunning, team.TaskStatusReady, team.TaskStatusClaimed,
		team.TaskStatusPending, team.TaskStatusCompleted, team.TaskStatusFailed,
		team.TaskStatusCancelled,
	}

	counts := make(map[team.TaskStatus]int, len(order))
	for _, taskValue := range tasks {
		counts[taskValue.Status]++
	}

	parts := make([]string, 0, len(order))
	for _, status := range order {
		if counts[status] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", status, counts[status]))
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return " · " + strings.Join(parts, " · ")
}

func teamAttemptSummary(attempt coding.TeamAttemptView) string {
	activity := string(attempt.Activity)
	if activity == "" {
		activity = string(attempt.LifecycleState)
	}

	if activity == "" {
		activity = string(attempt.DomainState)
	}

	if activity == "" {
		activity = unknownTeamRouteActivity
	}

	return fmt.Sprintf(
		"Attempt %d · %s · %s",
		attempt.Number,
		boundedTeamRouteText(activity, teamRoutePrimaryTextBytes),
		formatInteractionDuration(attempt.DurationMillis),
	)
}

func appendTeamRouteControls(
	lines []string,
	controls []coding.TeamControlView,
) []string {
	if len(controls) == 0 {
		return lines
	}

	lines = append(lines, "", "Recent controls")
	start := max(0, len(controls)-8)

	for _, control := range controls[start:] {
		value := fmt.Sprintf("  • %s · %s", control.Action, control.State)

		if control.Code != "" {
			value += " · " + boundedTeamRouteText(control.Code, teamRouteDetailTextBytes)
		}

		lines = append(lines, value)
	}

	return lines
}

func boundedTeamRouteText(value string, maximum int) string {
	value = strings.Join(strings.Fields(sanitizeInspectionText(value)), " ")

	return truncateText(value, maximum)
}
