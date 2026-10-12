//nolint:wsl_v5 // The pane's group, row, key and paint tables read as one inventory.
package tui

import (
	"fmt"
	"image/color"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

// tasksPaneGroup is one collapsible section of the tasks pane. The declaration
// order is the display order and the primary sort key.
type tasksPaneGroup uint8

const (
	tasksPaneWorkflows tasksPaneGroup = iota
	tasksPaneSubagents
	tasksPaneTasks
	tasksPaneWatchers

	tasksPaneGroupCount
)

// tasksPaneGroupLabels is the section vocabulary in display order. Only
// Subagents has a pips source today: pips runs no workflows, keeps no list of
// background commands, and has no watcher surface, so the other three groups
// contribute no header and no rows.
var tasksPaneGroupLabels = [tasksPaneGroupCount]string{
	"Workflows", "Subagents", "Tasks", "Watchers",
}

const (
	// tasksPaneMaxRows and tasksPaneHeightFraction cap the strip the way the
	// source caps its overlay, so the pane never takes the frame over.
	tasksPaneMaxRows        = 8
	tasksPaneHeightFraction = 0.15
	// tasksPaneMinViewHeight is the frame height below which the pane stays
	// hidden rather than crowding the conversation out.
	tasksPaneMinViewHeight = 12

	tasksPaneEmptyRunning = "No running tasks. Press h to show all."
	tasksPaneEmptyAll     = "No tasks or agents."
	// tasksPaneKillLabel and tasksPaneInspectLabel are the row's two controls, in
	// the order the source paints them: kill for a running row, then inspect.
	tasksPaneKillLabel    = "[✗]"
	tasksPaneInspectLabel = "[↗]"

	tasksPaneHeaderExpanded  = "▾"
	tasksPaneHeaderCollapsed = "▸"
)

// tasksPaneRow is one selectable line of the pane: a child of the current
// session, projected from the same record the timeline reads.
type tasksPaneRow struct {
	childID    string
	label      string
	activity   string
	model      string
	state      subagent.State
	durationMS int64
	running    bool
}

// tasksPaneEntry is one painted line: either a group header with its count or a
// selectable row.
type tasksPaneEntry struct {
	header bool
	group  tasksPaneGroup
	count  int
	// index is the row's position in the selectable list, which is what the
	// cursor addresses and what a click selects. A header carries -1.
	index int
	row   tasksPaneRow
}

// tasksPane is the model-local tasks overlay. It owns nothing durable: it reads
// the parent child projection and calls the existing control surface, so it can
// never become a second lifecycle model.
type tasksPane struct {
	visible      bool
	focused      bool
	openedByAuto bool
	showDone     bool
	collapsed    [tasksPaneGroupCount]bool
	cursor       int
	prevRunning  int
	// seen is when this process first observed each child running. It is the only
	// start time a running child has: the parent projection carries a duration
	// only once the child stops.
	seen map[string]time.Time
}

// tasksPaneHitMap records where the last painted pane sat, so a click resolves
// against what the reader saw rather than against a re-derived layout.
type tasksPaneHitMap struct {
	painted bool
	top     int
	// rows maps a painted pane line to the entry it shows, clamped to the frame.
	rows []tasksPaneHitRow
}

type tasksPaneHitRow struct {
	header bool
	group  tasksPaneGroup
	index  int
	// killStart and killEnd are the frame columns of the kill control, or -1 when
	// the row painted none; inspectStart and inspectEnd are the inspect control.
	killStart    int
	killEnd      int
	inspectStart int
	inspectEnd   int
}

// resetTasksPane drops the per-session observation state the pane keeps, so a
// navigation neither inherits the previous session's child records nor suppresses
// the next session's auto-open with a stale running count.
func (m *Model) resetTasksPane() {
	m.tasksPane = tasksPane{}
	m.tasksPaneHit = tasksPaneHitMap{}
}

// syncTasksPane applies the auto open/close rule after one frame of events and
// records when each running child was first observed.
func (m *Model) syncTasksPane(now time.Time) {
	m.applyTasksPaneVisibility(m.observeRunningChildren(now))
}

// observeRunningChildren records the children that are still running and drops
// the record of one that stopped, and reports how many are running.
func (m *Model) observeRunningChildren(now time.Time) int {
	if m.tasksPane.seen == nil {
		m.tasksPane.seen = make(map[string]time.Time)
	}

	running := 0
	for _, child := range m.state.Subagents {
		if child.ChildSessionID == "" {
			continue
		}
		if isTerminalSubagent(child.State) {
			delete(m.tasksPane.seen, child.ChildSessionID)

			continue
		}
		running++
		if _, seen := m.tasksPane.seen[child.ChildSessionID]; !seen {
			m.tasksPane.seen[child.ChildSessionID] = now
		}
	}

	return running
}

// applyTasksPaneVisibility opens the pane when a child starts and closes it with
// the last one, unless the reader opened it themselves, focused it, or asked for
// the finished rows.
func (m *Model) applyTasksPaneVisibility(running int) {
	pane := &m.tasksPane
	if running > 0 && pane.prevRunning == 0 {
		pane.visible = true
		pane.openedByAuto = true
	}
	if running == 0 && pane.openedByAuto && pane.visible && !pane.focused && !pane.showDone {
		pane.visible = false
		pane.openedByAuto = false
	}
	pane.prevRunning = running
}

// toggleTasksPane focuses the pane, revealing it if the automatic rule left it
// hidden, and unfocuses it on the second press.
func (m *Model) toggleTasksPane() tea.Cmd {
	wasVisible := m.activityClockVisible()
	if m.tasksPane.focused {
		m.tasksPane.focused = false

		return nil
	}
	m.revealTasksPane()
	m.tasksPane.focused = true
	m.clampTasksPaneCursor()

	return m.startActivityClock(wasVisible)
}

// revealTasksPane shows the pane without focusing it and takes it out of the
// automatic rule's hands, so the reader's own choice is not undone when the last
// child stops.
func (m *Model) revealTasksPane() {
	m.tasksPane.visible = true
	m.tasksPane.openedByAuto = false
}

// tasksPaneRunning reports whether any listed child is still running, which is
// what keeps the pane's spinner animating.
func (m *Model) tasksPaneRunning() bool {
	for _, row := range m.tasksPaneRows() {
		if row.running {
			return true
		}
	}

	return false
}

// tasksPaneRows projects every row the pane can show: the session's children, in
// the section order, running rows first. A finished row is listed only when the
// reader asked for finished rows.
func (m *Model) tasksPaneRows() []tasksPaneRow {
	rows := make([]tasksPaneRow, 0, len(m.state.Subagents))
	for _, child := range m.state.Subagents {
		if child.ChildSessionID == "" {
			continue
		}
		running := child.State == subagent.StateCreated || child.State == subagent.StateRunning
		if !running && !m.tasksPane.showDone {
			continue
		}
		rows = append(rows, tasksPaneRow{
			childID:    child.ChildSessionID,
			label:      tasksPaneRowLabel(child),
			activity:   subagentLiveActivityLabel(child),
			model:      safeDetailText(child.Model),
			state:      child.State,
			durationMS: child.DurationMillis,
			running:    running,
		})
	}

	sort.SliceStable(rows, func(left, right int) bool {
		if rows[left].running != rows[right].running {
			return rows[left].running
		}

		return rows[left].label < rows[right].label
	})

	return rows
}

func tasksPaneRowLabel(child coding.SubagentState) string {
	if preview := oneLineSubagentTask(child.TaskPreview); preview != "" {
		return preview
	}

	return subagentDisplayName(child.Identity, child.Role)
}

// tasksPaneGroupOf reports the section a row belongs to. Every pips row is a
// subagent today; the indirection is what a second row source would extend.
func tasksPaneGroupOf(tasksPaneRow) tasksPaneGroup {
	return tasksPaneSubagents
}

// tasksPaneEntries inserts a header before each non-empty group and numbers the
// rows in paint order, which is the order the cursor and a click address.
func tasksPaneEntries(rows []tasksPaneRow, collapsed [tasksPaneGroupCount]bool) []tasksPaneEntry {
	entries := make([]tasksPaneEntry, 0, len(rows)+int(tasksPaneGroupCount))
	index := 0
	for groupIndex := range int(tasksPaneGroupCount) {
		group := tasksPaneGroup(groupIndex)
		groupRows := make([]tasksPaneRow, 0, len(rows))
		for _, row := range rows {
			if tasksPaneGroupOf(row) == group {
				groupRows = append(groupRows, row)
			}
		}
		if len(groupRows) == 0 {
			continue
		}
		entries = append(entries, tasksPaneEntry{header: true, group: group, count: len(groupRows), index: -1})
		if collapsed[group] {
			continue
		}
		for _, row := range groupRows {
			entries = append(entries, tasksPaneEntry{group: group, index: index, row: row})
			index++
		}
	}

	return entries
}

// tasksPaneCapacity is how many rows the strip may paint at one frame height.
func (m *Model) tasksPaneCapacity() int {
	if m.height < tasksPaneMinViewHeight {
		return 0
	}
	fraction := int(float64(m.height) * tasksPaneHeightFraction)

	return max(1, min(tasksPaneMaxRows, fraction))
}

// tasksPaneVisible reports whether the pane paints in this frame.
func (m *Model) tasksPaneVisible() bool {
	if !m.tasksPane.visible || m.tasksPaneCapacity() == 0 {
		return false
	}

	return m.route.kind == routeNone || m.teamRouteIsInline()
}

// clampTasksPaneCursor keeps the selection inside the current row list.
func (m *Model) clampTasksPaneCursor() {
	if rows := m.tasksPaneRows(); len(rows) > 0 {
		m.tasksPane.cursor = min(max(0, m.tasksPane.cursor), len(rows)-1)

		return
	}

	m.tasksPane.cursor = 0
}

// tasksPaneBand renders the pane as the footer band it occupies, or reports an
// empty band when it is not painted.
func (m *Model) tasksPaneBand(now time.Time) string {
	if !m.tasksPaneVisible() {
		m.tasksPaneHit = tasksPaneHitMap{}

		return ""
	}

	painted, rows := m.tasksPanePaint(now, m.tasksPaneCapacity())
	m.tasksPaneHit = tasksPaneHitMap{painted: true, rows: rows}

	return painted
}

// tasksPanePaint renders the pane window and reports the entry each painted line
// shows, so the pointer map measures the same rows the frame paints.
func (m *Model) tasksPanePaint(now time.Time, capacity int) (string, []tasksPaneHitRow) {
	width := max(1, m.width)
	entries := tasksPaneEntries(m.tasksPaneRows(), m.tasksPane.collapsed)
	if len(entries) == 0 {
		text := tasksPaneEmptyRunning
		if m.tasksPane.showDone {
			text = tasksPaneEmptyAll
		}

		return m.tasksPaneMuted(text, width), []tasksPaneHitRow{{index: -1, killStart: -1, inspectStart: -1}}
	}

	window, hits := tasksPaneWindow(entries, m.tasksPane.cursor, capacity)
	lines := make([]string, 0, len(window))
	for index, entry := range window {
		if entry.header {
			hits[index] = tasksPaneHitRow{
				header: true, group: entry.group, index: -1,
				killStart: -1, inspectStart: -1,
			}
			lines = append(lines, m.tasksPaneHeaderLine(entry, width))

			continue
		}
		line, hit := m.tasksPaneRowLine(entry, now, width)
		hits[index] = hit
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n"), hits
}

// tasksPaneWindow selects the entries one frame paints and the pointer rows that
// go with them. The window follows the selection so a long list never hides the
// row the cursor is on.
func tasksPaneWindow(
	entries []tasksPaneEntry,
	cursor, capacity int,
) ([]tasksPaneEntry, []tasksPaneHitRow) {
	window := entries
	if capacity > 0 && len(entries) > capacity {
		selected := 0
		for index, entry := range entries {
			if !entry.header && entry.index == cursor {
				selected = index

				break
			}
		}
		start := 0
		if selected >= capacity {
			start = selected - capacity + 1
		}
		start = min(start, len(entries)-capacity)
		window = entries[start : start+capacity]
	}

	hits := make([]tasksPaneHitRow, len(window))
	for index := range hits {
		hits[index] = tasksPaneHitRow{index: -1, killStart: -1, inspectStart: -1}
	}

	return window, hits
}

func (m *Model) tasksPaneHeaderLine(entry tasksPaneEntry, width int) string {
	chevron := tasksPaneHeaderExpanded
	if m.tasksPane.collapsed[entry.group] {
		chevron = tasksPaneHeaderCollapsed
	}
	line := ansi.Truncate(
		fmt.Sprintf("%s %s %d", chevron, tasksPaneGroupLabels[entry.group], entry.count),
		width, "…",
	)
	if m.options.NoColor {
		return line
	}

	return lipgloss.NewStyle().
		Bold(true).
		Foreground(paletteFor(m.theme).idle).
		Render(line)
}

// tasksPaneRowLine paints one row and reports the frame columns its two controls
// occupy, so a click on either resolves to the action it promises.
func (m *Model) tasksPaneRowLine(
	entry tasksPaneEntry,
	now time.Time,
	width int,
) (string, tasksPaneHitRow) {
	hit := tasksPaneHitRow{index: entry.index, killStart: -1, inspectStart: -1}

	marker := "  "
	if m.tasksPane.focused && entry.index == m.tasksPane.cursor {
		marker = "› "
	}
	icon := m.tasksPaneRowIcon(entry.row)
	kill := ""
	if entry.row.running {
		kill = tasksPaneKillLabel
	}
	meta := strings.TrimSpace(strings.Join(
		nonEmpty(m.tasksPaneRowMeta(entry.row, now), kill, tasksPaneInspectLabel), "  ",
	))

	// The label gives up whatever the meta needs, so the two controls always sit
	// at the right edge where the reader expects to aim at them.
	labelWidth := max(0, width-ansi.StringWidth(meta)-ansi.StringWidth(marker)-ansi.StringWidth(icon)-1)
	label := ansi.Truncate(entry.row.label, labelWidth, "…")
	left := marker + icon + " " + label
	padding := strings.Repeat(" ", max(0, width-ansi.StringWidth(left)-ansi.StringWidth(meta)))
	line := ansi.Truncate(left+padding+meta, width, "…")

	// The controls are only where they appear to be when the row painted whole.
	if ansi.StringWidth(meta) <= width {
		hit.inspectEnd = width
		hit.inspectStart = max(0, width-ansi.StringWidth(tasksPaneInspectLabel))
		if kill != "" {
			hit.killEnd = max(0, hit.inspectStart-1)
			hit.killStart = max(0, hit.killEnd-ansi.StringWidth(tasksPaneKillLabel))
		}
	}

	if m.options.NoColor {
		return line, hit
	}

	palette := paletteFor(m.theme)
	styled := lipgloss.NewStyle().Foreground(palette.idle).Render(marker) +
		lipgloss.NewStyle().Foreground(m.tasksPaneRowTone(entry.row)).Render(icon) +
		lipgloss.NewStyle().Foreground(palette.workspace).Render(" "+label) +
		padding +
		lipgloss.NewStyle().Foreground(palette.muted).Render(meta)

	return styled, hit
}

// tasksPaneRowTone is the state color the row's icon carries: running, finished,
// failed, or interrupted.
func (m *Model) tasksPaneRowTone(row tasksPaneRow) color.Color {
	palette := paletteFor(m.theme)
	switch subagentToolState(row.state) {
	case toolStateRunning:
		return palette.active
	case toolStateFailed:
		return palette.error
	case toolStateInterrupted:
		return palette.warning
	default:
		return palette.muted
	}
}

// tasksPaneRowIcon is the row's state mark: the shared spinner while the child
// runs, and a settled mark once it stops.
func (m *Model) tasksPaneRowIcon(row tasksPaneRow) string {
	switch state := subagentToolState(row.state); state {
	case toolStateSucceeded, toolStateFailed, toolStateInterrupted:
		return toolActivityGlyph(state)
	default:
		return m.activity.Frame()
	}
}

// tasksPaneRowMeta is the right-hand meta of a row: the elapsed time, the
// child's live activity when it has one, and the model.
func (m *Model) tasksPaneRowMeta(row tasksPaneRow, now time.Time) string {
	elapsed := formatInteractionDuration(row.durationMS)
	if row.running {
		elapsed = ""
		if seen, ok := m.tasksPane.seen[row.childID]; ok && !now.IsZero() {
			elapsed = formatInteractionDuration(now.Sub(seen).Milliseconds())
		}
	}

	return strings.Join(nonEmpty(elapsed, row.activity, row.model), "  ")
}

func (m *Model) tasksPaneMuted(text string, width int) string {
	line := ansi.Truncate(text, width, "…")
	if m.options.NoColor {
		return line
	}

	return lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(line)
}

// nonEmpty drops the parts that paint nothing, so the join controls its own
// separators.
func nonEmpty(parts ...string) []string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}

	return kept
}

// updateTasksPaneReadyKey gives the pane the ready view's keys it owns: one
// chord reveals and focuses it, and while focused it owns the frame's keys the
// way any other focused overlay does.
func (m *Model) updateTasksPaneReadyKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	if message.String() == keyCtrlB {
		return m, m.toggleTasksPane(), true
	}
	if m.tasksPane.focused {
		model, command := m.updateTasksPaneKey(message)

		return model, command, true
	}

	return m, nil, false
}

// updateTasksPaneKey drives the focused pane.
func (m *Model) updateTasksPaneKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key := message.String(); key {
	case keyTab, keyEscape, keyCtrlB:
		m.tasksPane.focused = false

		return m, nil
	case "up", "k":
		m.moveTasksPaneCursor(-1)

		return m, nil
	case keyDown, "j":
		m.moveTasksPaneCursor(1)

		return m, nil
	case "h":
		m.tasksPane.showDone = !m.tasksPane.showDone
		m.clampTasksPaneCursor()

		return m, nil
	case keyEnter, keyCtrlF:
		return m, m.inspectTasksPaneSelection()
	case "x":
		return m, m.killTasksPaneSelection()
	case keyLeft:
		m.setTasksPaneCollapsed(true)

		return m, nil
	case keyRight:
		m.setTasksPaneCollapsed(false)

		return m, nil
	case keyHome:
		m.tasksPane.cursor = 0

		return m, nil
	case keyEnd:
		m.tasksPane.cursor = max(0, len(m.tasksPaneRows())-1)

		return m, nil
	}

	return m, nil
}

// moveTasksPaneCursor moves the selection by one row, clamped at both ends.
func (m *Model) moveTasksPaneCursor(delta int) {
	rows := m.tasksPaneRows()
	if len(rows) == 0 {
		m.tasksPane.cursor = 0

		return
	}
	m.tasksPane.cursor = min(max(0, m.tasksPane.cursor+delta), len(rows)-1)
}

// setTasksPaneCollapsed collapses or expands every section.
func (m *Model) setTasksPaneCollapsed(collapsed bool) {
	for index := range m.tasksPane.collapsed {
		m.tasksPane.collapsed[index] = collapsed
	}
}

// tasksPaneSelection resolves the selected row, or reports that there is none.
func (m *Model) tasksPaneSelection() (tasksPaneRow, bool) {
	rows := m.tasksPaneRows()
	if len(rows) == 0 {
		return tasksPaneRow{}, false
	}
	index := min(max(0, m.tasksPane.cursor), len(rows)-1)

	return rows[index], true
}

// inspectTasksPaneSelection opens the selected child's transcript. It is the
// same route Ctrl+T opens, so there is one child surface rather than two.
func (m *Model) inspectTasksPaneSelection() tea.Cmd {
	row, ok := m.tasksPaneSelection()
	if !ok {
		return nil
	}

	return m.openSubagentRoute(row.childID)
}

// killTasksPaneSelection cancels the selected child through the existing control
// surface. A row that is not running affords nothing.
func (m *Model) killTasksPaneSelection() tea.Cmd {
	row, ok := m.tasksPaneSelection()
	if !ok || !row.running || m.route.controlling {
		return nil
	}

	return m.cancelSubagent(row.childID)
}

// tasksPaneMouse resolves a pointer gesture against the painted pane. It only
// consumes what it affords: a click on a control fires that control's action, a
// click on a header toggles its section, and any other click inside the pane
// selects the row it landed on. A gesture that misses the pane's rows stays the
// transcript's, so a drag selection is not cut short by the strip.
func (m *Model) tasksPaneMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	if !m.tasksPaneHit.painted || !m.tasksPaneVisible() {
		return nil, false
	}

	click, ok := message.(tea.MouseClickMsg)
	if !ok || click.Button != tea.MouseLeft {
		return nil, m.tasksPaneOwnsRow(message.Mouse().Y)
	}

	hit, ok := m.tasksPaneHitRowAt(click.Y)
	if !ok {
		return nil, false
	}

	return m.tasksPaneHitCommand(hit, click.X)
}

// tasksPaneOwnsRow reports whether a frame row falls inside the painted pane, so
// a pointer gesture that misses it is left to the transcript underneath.
func (m *Model) tasksPaneOwnsRow(row int) bool {
	if m.tasksPaneHit.top < 0 {
		return false
	}

	line := row - m.tasksPaneHit.top

	return line >= 0 && line < len(m.tasksPaneHit.rows)
}

// tasksPaneHitRowAt resolves a frame row to the pane line it painted.
func (m *Model) tasksPaneHitRowAt(row int) (tasksPaneHitRow, bool) {
	line := row - m.tasksPaneHit.top
	if line < 0 || line >= len(m.tasksPaneHit.rows) {
		return tasksPaneHitRow{}, false
	}

	return m.tasksPaneHit.rows[line], true
}

// tasksPaneHitCommand runs what one painted pane line affords at a frame column.
func (m *Model) tasksPaneHitCommand(hit tasksPaneHitRow, column int) (tea.Cmd, bool) {
	if hit.header {
		m.tasksPane.collapsed[hit.group] = !m.tasksPane.collapsed[hit.group]

		return redrawFrame(), true
	}
	rows := m.tasksPaneRows()
	if hit.index < 0 || hit.index >= len(rows) {
		return nil, false
	}
	row := rows[hit.index]

	if inColumnSpan(column, hit.killStart, hit.killEnd) && row.running {
		return m.cancelSubagent(row.childID), true
	}
	if inColumnSpan(column, hit.inspectStart, hit.inspectEnd) {
		return m.openSubagentRoute(row.childID), true
	}
	m.tasksPane.cursor = hit.index
	m.tasksPane.focused = true

	return redrawFrame(), true
}

// inColumnSpan reports whether a frame column falls inside a painted control.
// A control the row did not paint carries a negative start and answers nothing.
func inColumnSpan(column, start, end int) bool {
	return start >= 0 && column >= start && column < end
}
