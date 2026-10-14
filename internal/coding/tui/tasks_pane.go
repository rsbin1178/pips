//nolint:wsl_v5 // The pane's group, row, key and paint tables read as one inventory.
package tui

import (
	"fmt"
	"image/color"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
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

// tasksPaneGroupLabels is the section vocabulary in display order. Three groups
// have a pips source today: Subagents projects the session's children, Tasks
// projects the task plan, and Watchers projects its registered background tasks.
// pips runs no workflows and keeps no list of one-shot background commands, so
// those contribute no header and no rows.
var tasksPaneGroupLabels = [tasksPaneGroupCount]string{
	"Workflows", "Subagents", "Tasks", "Watchers",
}

const (
	// tasksPaneMaxRows caps the rows the pane may paint, and
	// tasksPaneMaxHeightFraction caps them as a share of the frame. The pane is a
	// compact strip and the transcript is the frame's only elastic band, so the
	// cap is tight: a section still names itself, shows its rows and counts the
	// ones the cap hides, while the conversation keeps most of its rows.
	tasksPaneMaxRows           = 6
	tasksPaneMaxHeightFraction = 0.30
	// tasksPaneMinViewHeight is the frame height below which the pane stays
	// hidden rather than crowding the conversation out.
	tasksPaneMinViewHeight = 12

	// tasksPaneMorePrefix and tasksPaneMoreSuffix word the line a squeezed
	// section spends its last granted row on.
	tasksPaneMorePrefix = "… +"
	tasksPaneMoreSuffix = " more"

	// tasksPaneKillLabel and tasksPaneInspectLabel are the row's two controls, in
	// the order the source paints them: kill for a running row, then inspect.
	tasksPaneKillLabel    = "[✗]"
	tasksPaneInspectLabel = "[↗]"

	tasksPaneHeaderExpanded  = "▾"
	tasksPaneHeaderCollapsed = "▸"
	// backgroundTaskTag labels a watch row and backgroundCommandTag labels a
	// backgrounded command's row, so neither is read as the other.
	backgroundTaskTag    = "Monitor"
	backgroundCommandTag = "Command"
)

// backgroundTaskTagFor returns the row tag for one background task kind. The
// tag text lives here alone, so a command and a watch are never read as each
// other.
func backgroundTaskTagFor(kind string) string {
	if kind == string(background.KindShell) {
		return backgroundCommandTag
	}

	return backgroundTaskTag
}

// tasksPaneRow is one selectable line of the pane: a child of the current
// session, or a background task the session registered. Both are projections
// of the same runtime state the timeline reads.
type tasksPaneRow struct {
	// childID addresses a subagent row and taskID a background-task row; exactly
	// one is set, and it is what the row's two actions address.
	childID string
	taskID  string
	// plan marks a row projected from the task plan rather than a child or a
	// background task. A plan row addresses no action, and planStatus is the
	// step's own status.
	plan       bool
	planStatus tasklist.Status
	// tag labels a row whose source is not a child, so a watch is never read as
	// one.
	tag        string
	label      string
	activity   string
	model      string
	state      subagent.State
	durationMS int64
	running    bool
	// startedAt and endedAt are a background task's own clock. A child's elapsed
	// time comes from durationMS, or from first observation while it runs.
	startedAt time.Time
	endedAt   time.Time
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
	m.applyTasksPaneVisibility(m.observeRunningRows(now))
}

// observeRunningRows records the children that are still running and drops the
// record of one that stopped, and reports how many rows are running. A running
// background task counts too: an automatically-opened pane that hid a watch
// would leave the reader no way to see it.
func (m *Model) observeRunningRows(now time.Time) int {
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

	for _, task := range m.state.BackgroundTasks {
		if task.ID != "" && !task.Completed {
			running++
		}
	}

	return running
}

// stillRunningKind is one count and its noun in the "… still running" cue.
type stillRunningKind struct {
	count int
	noun  string
}

// formatStillRunning is the single owner of the cue's format mechanics: only
// non-zero kinds, plain-`s` plurals, " · " separators, and the " still running"
// suffix, matching the reference's format_still_running. Keeping the writer in
// one place is what stops the status line's idle cue and any later surface that
// shows the same counts from drifting.
func formatStillRunning(kinds []stillRunningKind) string {
	var label strings.Builder

	for _, kind := range kinds {
		if kind.count == 0 {
			continue
		}
		if label.Len() > 0 {
			label.WriteString(" · ")
		}
		label.WriteString(strconv.Itoa(kind.count))
		label.WriteString(" ")
		label.WriteString(kind.noun)
		if kind.count != 1 {
			label.WriteString("s")
		}
	}
	if label.Len() == 0 {
		return ""
	}
	label.WriteString(" still running")

	return label.String()
}

// runningWorkCounts counts the live kinds the cue and the pane both read:
// background commands, background watches (pips' Watchers), and running
// children (Subagents). The reference's cue also lists loops and workflows;
// pips runs none of those, so they are absent. The child predicate matches the
// pane's running rows, so the two surfaces cannot disagree about what runs.
func (m *Model) runningWorkCounts() (commands, monitors, subagents int) {
	for _, task := range m.state.BackgroundTasks {
		if task.ID == "" || task.Completed {
			continue
		}

		if task.Kind == string(background.KindShell) {
			commands++
		} else {
			monitors++
		}
	}

	for _, child := range m.state.Subagents {
		if child.ChildSessionID == "" {
			continue
		}
		if child.State == subagent.StateCreated || child.State == subagent.StateRunning {
			subagents++
		}
	}

	return commands, monitors, subagents
}

// stillRunningCue renders the count-led cue the reference shows while its agent
// is idle (or parked on a sendable wait, which pips has none of) and something
// is still running. It stays empty while a turn runs and while the runtime waits
// for the user, so the cue never competes with the live status. It is
// deliberately count-led, so it still reads as unfinished work under a settled
// transcript.
func (m *Model) stillRunningCue() string {
	if m.state.Phase != coding.PhaseIdle {
		return ""
	}
	commands, monitors, subagents := m.runningWorkCounts()

	return formatStillRunning([]stillRunningKind{
		{count: commands, noun: "command"},
		{count: monitors, noun: "monitor"},
		{count: subagents, noun: "subagent"},
	})
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
	// A pane with nothing to list owns no keys, and it does not take them back on
	// its own: the focus is dropped with the rows, so a child starting while the
	// reader types cannot silently take a keystroke meant for the composer.
	if pane.focused && !m.tasksPaneHasEntries() {
		pane.focused = false
	}
	pane.prevRunning = running
}

// toggleTasksPane focuses the pane, revealing it if the automatic rule left it
// hidden, and unfocuses it on the second press. A pane that has nothing to list
// owns no keys, so the chord reveals it again instead of unfocusing it.
func (m *Model) toggleTasksPane() tea.Cmd {
	wasVisible := m.activityClockVisible()
	if m.tasksPaneFocused() {
		m.tasksPane.focused = false

		return nil
	}
	m.revealTasksPane()
	m.tasksPane.focused = true
	m.clampTasksPaneCursor()

	return m.startActivityClock(wasVisible)
}

// toggleTasksPaneHistory answers the reader's "show me what has run" gesture: it
// turns the finished rows on, revealing and focusing the pane for them, and puts
// them away again on the second press. The pane is usually hidden by then, since
// the automatic rule closes it when the last child or watch stops — which is
// exactly when its history is wanted — so the chord is answered from the
// conversation rather than only from the pane's own keys.
func (m *Model) toggleTasksPaneHistory() tea.Cmd {
	wasVisible := m.activityClockVisible()
	if m.tasksPane.showDone && m.tasksPaneVisible() {
		m.tasksPane.showDone = false
		m.tasksPane.focused = false

		return nil
	}
	m.tasksPane.showDone = true
	m.revealTasksPane()
	m.tasksPane.focused = true
	m.clampTasksPaneCursor()

	return m.startActivityClock(wasVisible)
}

// revealTasksPane shows the pane without focusing it and takes it out of the
// automatic rule's hands, so the reader's own choice is not undone when the last
// child stops. Revealing with nothing running turns the finished rows on: the
// reader asked to see the pane, and an empty strip is not an answer.
func (m *Model) revealTasksPane() {
	if !m.tasksPaneRunning() {
		m.tasksPane.showDone = true
	}
	m.tasksPane.visible = true
	m.tasksPane.openedByAuto = false
}

// tasksPaneFocused reports whether the pane owns the frame's keys and paints its
// selection marker. An empty pane owns nothing: it has no rows to move and no
// control to fire, so the composer keeps the frame's keys.
func (m *Model) tasksPaneFocused() bool {
	return m.tasksPane.focused && m.tasksPaneVisible()
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

// tasksPaneRows projects every row the pane can show: the session's children,
// its background tasks, and the projected task plan, in the section order,
// running rows first. A finished row is listed only when the reader asked for
// finished rows.
func (m *Model) tasksPaneRows() []tasksPaneRow {
	rows := make([]tasksPaneRow, 0, len(m.state.Subagents)+len(m.state.BackgroundTasks)+len(m.state.Tasks.Items))
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

	for _, task := range m.state.BackgroundTasks {
		if row, ok := tasksPaneBackgroundRow(task, m.tasksPane.showDone); ok {
			rows = append(rows, row)
		}
	}

	sort.SliceStable(rows, func(left, right int) bool {
		if rows[left].running != rows[right].running {
			return rows[left].running
		}

		return rows[left].label < rows[right].label
	})

	// Plan rows keep the plan's own order, so they are appended after the sort
	// rather than alphabetized like the live rows.
	for _, item := range m.state.Tasks.Items {
		if row, ok := tasksPanePlanRow(item, m.tasksPane.showDone); ok {
			rows = append(rows, row)
		}
	}

	return rows
}

// tasksPanePlanRow projects one plan item into a row. A finished step follows
// the same finished-rows rule as the other groups: it is listed only when the
// reader asked for finished rows, so a fully finished plan paints no section at
// all until then.
func tasksPanePlanRow(item tasklist.Item, showDone bool) (tasksPaneRow, bool) {
	label := oneLineToolText(item.Step)
	if label == "" {
		return tasksPaneRow{}, false
	}
	finished := item.Status == tasklist.StatusCompleted || item.Status == tasklist.StatusCancelled
	if finished && !showDone {
		return tasksPaneRow{}, false
	}

	return tasksPaneRow{
		plan:       true,
		planStatus: item.Status,
		label:      label,
		activity:   planItemStatusText(item.Status),
		state:      planItemState(item.Status),
	}, true
}

// planItemStatusText names one plan item's status for the pane's meta column.
func planItemStatusText(status tasklist.Status) string {
	switch status {
	case tasklist.StatusInProgress:
		return "in progress"
	case tasklist.StatusCompleted:
		return "completed"
	case tasklist.StatusCancelled:
		return "cancelled"
	default:
		return "pending"
	}
}

// planItemState maps a plan status onto the child states the pane already tones
// rows by, so a plan row reuses the shared palette.
func planItemState(status tasklist.Status) subagent.State {
	switch status {
	case tasklist.StatusInProgress:
		return subagent.StateRunning
	case tasklist.StatusCompleted:
		return subagent.StateSucceeded
	case tasklist.StatusCancelled:
		return subagent.StateCanceled
	default:
		return subagent.StateSucceeded
	}
}

// tasksPaneBackgroundRow projects one registered background task into a row. A
// finished task is listed only when the reader asked for finished rows.
func tasksPaneBackgroundRow(
	task coding.BackgroundTaskLifecycle,
	showDone bool,
) (tasksPaneRow, bool) {
	if task.ID == "" {
		return tasksPaneRow{}, false
	}

	running := !task.Completed
	if !running && !showDone {
		return tasksPaneRow{}, false
	}

	return tasksPaneRow{
		taskID: task.ID, tag: backgroundTaskTagFor(task.Kind),
		label:     backgroundTaskRowLabel(task),
		state:     backgroundTaskState(task),
		running:   running,
		startedAt: task.StartedAt, endedAt: task.EndedAt,
	}, true
}

// backgroundTaskState maps a task's own outcome onto the states the pane already
// paints, so a watch reuses the child rows' spinner and settled marks.
func backgroundTaskState(task coding.BackgroundTaskLifecycle) subagent.State {
	if !task.Completed {
		return subagent.StateRunning
	}
	if task.Signal == "" && task.ExitCode == 0 {
		return subagent.StateSucceeded
	}

	return subagent.StateFailed
}

// backgroundTaskRowLabel is the row text for one background task: the
// description with newlines flattened, else the command.
func backgroundTaskRowLabel(task coding.BackgroundTaskLifecycle) string {
	if text := oneLineToolText(task.Description); text != "" {
		return text
	}

	return oneLineToolText(task.Command)
}

// backgroundTaskStatusText names one task's outcome for a detail heading.
func backgroundTaskStatusText(task coding.BackgroundTaskLifecycle) string {
	switch {
	case !task.Completed:
		return "running"
	case task.Signal != "":
		return "killed by signal " + task.Signal
	case task.ExitCode != 0:
		return "exited (code " + strconv.Itoa(task.ExitCode) + ")"
	default:
		return "exited (code 0)"
	}
}

// backgroundTaskElapsedText renders one task's own elapsed time.
func backgroundTaskElapsedText(task coding.BackgroundTaskLifecycle, now time.Time) string {
	if task.StartedAt.IsZero() {
		return ""
	}
	end := task.EndedAt
	if end.IsZero() {
		end = now
	}

	return formatInteractionDuration(end.Sub(task.StartedAt).Milliseconds())
}

func tasksPaneRowLabel(child coding.SubagentState) string {
	if preview := oneLineSubagentTask(child.TaskPreview); preview != "" {
		return preview
	}

	return subagentDisplayName(child.Identity, child.Role)
}

// tasksPaneGroupOf reports the section a row belongs to. A child is a
// Subagent, a session-registered background task is a Watcher, and a projected
// plan item is a Task.
func tasksPaneGroupOf(row tasksPaneRow) tasksPaneGroup {
	switch {
	case row.plan:
		return tasksPaneTasks
	case row.taskID != "":
		return tasksPaneWatchers
	default:
		return tasksPaneSubagents
	}
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

// tasksPaneRowWidth is the row width the pane paints at: the whole frame, so a
// row's two controls stay at the right edge where the reader aims at them.
func (m *Model) tasksPaneRowWidth() int {
	return max(1, m.width)
}

// tasksPaneHasEntries reports whether the pane has anything to list. An empty pane
// has no capacity, so it paints nothing and owns no keys.
func (m *Model) tasksPaneHasEntries() bool {
	return len(tasksPaneEntries(m.tasksPaneRows(), m.tasksPane.collapsed)) > 0
}

// tasksPaneCapacity is how many rows the pane may paint: what its sections need,
// bounded by the row cap and by the pane's share of the frame, so a crowded list
// still leaves the conversation most of its rows. No entries means no capacity:
// a pane with nothing to list paints nothing at all.
func (m *Model) tasksPaneCapacity() int {
	if m.height < tasksPaneMinViewHeight {
		return 0
	}

	needed := len(tasksPaneEntries(m.tasksPaneRows(), m.tasksPane.collapsed))
	if needed == 0 {
		return 0
	}
	share := max(1, int(float64(m.height)*tasksPaneMaxHeightFraction))

	return max(1, min(min(needed, tasksPaneMaxRows), share))
}

// tasksPaneVisible reports whether the pane paints in this frame. An empty pane
// has no capacity, so it is not visible: the band contributes no row and the
// composer keeps the frame's keys.
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

// tasksPaneBand renders the pane as the frame's last band: the rows below the
// status line, or an empty band when the pane is not painted.
func (m *Model) tasksPaneBand(now time.Time) string {
	if !m.tasksPaneVisible() {
		m.tasksPaneHit = tasksPaneHitMap{}

		return ""
	}

	painted, rows := m.tasksPanePaint(now, m.tasksPaneCapacity())
	m.tasksPaneHit = tasksPaneHitMap{painted: true, rows: rows}

	return painted
}

// tasksPaneSection is one group's run of entries: its header and the rows under
// it, which is the unit the container grants rows to.
type tasksPaneSection struct {
	header tasksPaneEntry
	rows   []tasksPaneEntry
}

// tasksPaneSections splits the painted entries into their sections. A section is
// always a header with the rows that follow it, which is what lets a squeezed
// container drop a whole section instead of painting a header over nothing.
func tasksPaneSections(entries []tasksPaneEntry) []tasksPaneSection {
	sections := make([]tasksPaneSection, 0, int(tasksPaneGroupCount))

	for _, entry := range entries {
		if entry.header {
			sections = append(sections, tasksPaneSection{header: entry})

			continue
		}
		if len(sections) == 0 {
			continue
		}

		last := len(sections) - 1
		sections[last].rows = append(sections[last].rows, entry)
	}

	return sections
}

// tasksPaneGrants hands every section its floor — a header and one row — before
// any section takes a second row, so a container that cannot hold the whole list
// still names each section it lists. The remaining rows are shared one at a time
// in display order, so a long first section cannot starve the ones after it.
func tasksPaneGrants(sections []tasksPaneSection, capacity int) []int {
	grants := make([]int, len(sections))
	remaining := capacity

	for index, section := range sections {
		needed := 1
		if len(section.rows) > 0 {
			needed = 2
		}
		if remaining < needed {
			break
		}

		grants[index] = needed
		remaining -= needed
	}

	for remaining > 0 {
		shared := false

		for index, section := range sections {
			if remaining == 0 {
				break
			}
			if grants[index] == 0 || grants[index] > len(section.rows) {
				continue
			}

			grants[index]++
			remaining--
			shared = true
		}
		if !shared {
			break
		}
	}

	return grants
}

// tasksPaneSectionWindow picks the rows one section paints in the room it was
// granted, and reports how many it hides. A section that hides rows spends its
// last granted row on its own overflow line and scrolls the rest, so the row the
// reader is on stays visible however long the section is.
func tasksPaneSectionWindow(rows []tasksPaneEntry, cursor, room int) ([]tasksPaneEntry, int) {
	if room <= 0 {
		return nil, len(rows)
	}
	if len(rows) <= room {
		return rows, 0
	}

	painted := max(1, room-1)
	selected := 0

	for index, row := range rows {
		if row.index == cursor {
			selected = index

			break
		}
	}

	start := min(max(0, selected-painted+1), len(rows)-painted)

	return rows[start : start+painted], len(rows) - painted
}

// tasksPaneCursorRow is the entry the cursor addresses, or the first row of the
// list when it addresses nothing.
func tasksPaneCursorRow(sections []tasksPaneSection, cursor int) (tasksPaneEntry, bool) {
	for _, section := range sections {
		for _, row := range section.rows {
			if row.index == cursor {
				return row, true
			}
		}
	}

	for _, section := range sections {
		if len(section.rows) > 0 {
			return section.rows[0], true
		}
	}

	return tasksPaneEntry{}, false
}

// tasksPanePaint renders the pane's rows and the pointer row for each painted
// line. Sections are painted in display order, a header always has at least one
// of its rows under it, and a section whose rows do not all fit says how many it
// hides. A pane with no section paints nothing: there is no empty-state note,
// because an empty pane is not visible at all.
func (m *Model) tasksPanePaint(now time.Time, capacity int) (string, []tasksPaneHitRow) {
	width := m.tasksPaneRowWidth()
	sections := tasksPaneSections(tasksPaneEntries(m.tasksPaneRows(), m.tasksPane.collapsed))
	if len(sections) == 0 || capacity <= 0 {
		return "", nil
	}

	// One row of room goes to the row the reader is on: a header would count rows
	// that are not painted, and the row is what both actions address.
	if capacity == 1 {
		if row, ok := tasksPaneCursorRow(sections, m.tasksPane.cursor); ok {
			line, hit := m.tasksPaneRowLine(row, now, width)

			return line, []tasksPaneHitRow{hit}
		}
	}

	grants := tasksPaneGrants(sections, capacity)
	lines := make([]string, 0, capacity)
	hits := make([]tasksPaneHitRow, 0, capacity)

	for index, section := range sections {
		grant := grants[index]
		if grant == 0 {
			continue
		}

		header := section.header
		lines = append(lines, m.tasksPaneHeaderLine(header, width))
		hits = append(hits, tasksPaneHitRow{
			header: true, group: header.group, index: -1, killStart: -1, inspectStart: -1,
		})

		painted, hidden := tasksPaneSectionWindow(section.rows, m.tasksPane.cursor, grant-1)

		for _, row := range painted {
			line, hit := m.tasksPaneRowLine(row, now, width)
			lines = append(lines, line)
			hits = append(hits, hit)
		}

		if hidden > 0 {
			lines = append(lines, m.tasksPaneMuted(tasksPaneMoreLabel(hidden), width))
			hits = append(hits, tasksPaneHitRow{index: -1, killStart: -1, inspectStart: -1})
		}
	}

	return strings.Join(lines, "\n"), hits
}

// tasksPaneMoreLabel words how many rows one squeezed section hides.
func tasksPaneMoreLabel(hidden int) string {
	return tasksPaneMorePrefix + strconv.Itoa(hidden) + tasksPaneMoreSuffix
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
	if m.tasksPaneFocused() && entry.index == m.tasksPane.cursor {
		marker = "› "
	}
	icon := m.tasksPaneRowIcon(entry.row)
	kill := ""
	if entry.row.running {
		kill = tasksPaneKillLabel
	}
	// A plan row addresses no action, so it carries neither control; the two
	// controls are what the hit map resolves a click against.
	inspect := ""
	if !entry.row.plan {
		inspect = tasksPaneInspectLabel
	}
	meta := strings.TrimSpace(strings.Join(
		nonEmpty(m.tasksPaneRowMeta(entry.row, now), kill, inspect), "  ",
	))

	// The label gives up whatever the meta needs, so the two controls always sit
	// at the right edge where the reader expects to aim at them.
	labelWidth := max(0, width-ansi.StringWidth(meta)-ansi.StringWidth(marker)-ansi.StringWidth(icon)-1)
	label := ansi.Truncate(entry.row.label, labelWidth, "…")
	if entry.row.tag != "" {
		label = ansi.Truncate(entry.row.tag+" "+entry.row.label, labelWidth, "…")
	}
	left := marker + icon + " " + label
	padding := strings.Repeat(" ", max(0, width-ansi.StringWidth(left)-ansi.StringWidth(meta)))
	line := ansi.Truncate(left+padding+meta, width, "…")

	// The controls are only where they appear to be when the row painted whole.
	if ansi.StringWidth(meta) <= width && inspect != "" {
		hit.inspectEnd = width
		hit.inspectStart = max(0, width-ansi.StringWidth(inspect))
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
// runs, and a settled mark once it stops. A plan row carries the checklist
// glyph its own status uses, so the pane and the plan card read alike.
func (m *Model) tasksPaneRowIcon(row tasksPaneRow) string {
	if row.plan {
		return taskListGlyph(row.planStatus)
	}

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
	switch {
	case row.plan:
		// A plan step carries no clock: its meta is its status alone, so the
		// pane never fabricates an elapsed time for it.
		elapsed = ""
	case !row.startedAt.IsZero():
		// A background task carries its own start, so its clock does not depend
		// on when this process first observed it.
		end := row.endedAt
		if end.IsZero() {
			end = now
		}
		elapsed = formatInteractionDuration(end.Sub(row.startedAt).Milliseconds())
	case row.running:
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
// chord reveals and focuses it, one shows the rows that already finished, and
// while focused it owns the frame's keys the way any other focused overlay does.
// A pane with nothing to list owns nothing, so its keys fall through to the
// surfaces behind it.
func (m *Model) updateTasksPaneReadyKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	if message.String() == keyCtrlB {
		return m, m.toggleTasksPane(), true
	}
	if message.String() == keyCtrlH {
		return m, m.toggleTasksPaneHistory(), true
	}
	if m.tasksPaneFocused() {
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
	// A terminal profile that sends Ctrl+H as Backspace reaches the history
	// toggle here: the chord itself is answered before the pane's own keys.
	// ultraviolet decodes the bare 0x08 as ctrl+h and the 0x7f a modern Backspace
	// sends as backspace, and Backspace has no other job while the pane is focused
	// — the composer is blurred and the pane consumes every key.
	case keyBackspace:
		return m, m.toggleTasksPaneHistory()
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

// inspectTasksPaneSelection opens the selected row's bounded detail. A child
// opens its transcript through the one child route; a background task opens the
// same detail surface a tool call does, because a watch has no transcript; a
// plan row opens nothing.
func (m *Model) inspectTasksPaneSelection() tea.Cmd {
	row, ok := m.tasksPaneSelection()
	if !ok || row.plan {
		return nil
	}
	if row.taskID != "" {
		return m.inspectBackgroundTask(row.taskID)
	}

	return m.openSubagentRoute(row.childID)
}

// killTasksPaneSelection stops the selected row through the one control surface
// its kind uses. A row that is not running affords nothing, and a plan row
// affords no stop.
func (m *Model) killTasksPaneSelection() tea.Cmd {
	row, ok := m.tasksPaneSelection()
	if !ok || !row.running || row.plan || m.route.controlling {
		return nil
	}
	if row.taskID != "" {
		return m.killBackgroundTask(row.taskID)
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

// tasksPaneOwnsWheel reports whether a wheel notch landed inside the rows the
// pane painted. The pane is the frame's last band, so a notch above it still
// belongs to the transcript.
func (m *Model) tasksPaneOwnsWheel(message tea.MouseWheelMsg) bool {
	if !m.tasksPaneHit.painted || !m.tasksPaneVisible() {
		return false
	}

	return m.tasksPaneOwnsRow(message.Mouse().Y)
}

// scrollTasksPane moves the pane's selection by one notch through the pane's own
// key path, so clamping and its section windows stay one path. The gesture does
// not take the keyboard: a wheel points at the rows the way it points at any other
// list, while a click is what focuses the pane.
func (m *Model) scrollTasksPane(message tea.MouseWheelMsg) tea.Cmd {
	lines, ok := m.wheelLines(message)
	if !ok {
		return nil
	}

	return moveSelection(lines, m.updateTasksPaneKey)
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

	// A plan row affords neither control, so a click anywhere on it selects it.
	if row.plan {
		m.tasksPane.cursor = hit.index
		m.tasksPane.focused = true

		return redrawFrame(), true
	}

	if inColumnSpan(column, hit.killStart, hit.killEnd) && row.running {
		if row.taskID != "" {
			return m.killBackgroundTask(row.taskID), true
		}

		return m.cancelSubagent(row.childID), true
	}
	if inColumnSpan(column, hit.inspectStart, hit.inspectEnd) {
		if row.taskID != "" {
			return m.inspectBackgroundTask(row.taskID), true
		}

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

// backgroundKillResultMsg carries one stop answer back to the pane.
type backgroundKillResultMsg struct {
	taskID  string
	outcome background.Outcome
	err     error
}

// backgroundInspectResultMsg carries one bounded inspection back to the pane.
// follow marks a re-read the open watch page asked for, so the answer refreshes
// that page instead of opening a second one.
type backgroundInspectResultMsg struct {
	taskID     string
	inspection coding.BackgroundTaskInspection
	err        error
	follow     bool
}

// killBackgroundTask stops one watch through the same control method the model's
// own stop Tool uses. The pane's rows name a watch or a child and both settle
// from the same projection, so the answer's subject is not the pane's business.
func (m *Model) killBackgroundTask(taskID string) tea.Cmd {
	fetch := func() tea.Msg {
		outcome, _, err := m.controller.KillBackgroundTask(m.ctx, taskID)

		return backgroundKillResultMsg{taskID: taskID, outcome: outcome, err: err}
	}

	return fetch
}

// inspectBackgroundTask loads one watch's bounded detail and opens it.
func (m *Model) inspectBackgroundTask(taskID string) tea.Cmd {
	return m.fetchBackgroundTask(taskID, false)
}

// followBackgroundTask re-reads the watch the open page is showing. The page
// re-reads on the activity clock while the task runs, so the reader watches the
// output grow instead of a snapshot taken when they opened it.
func (m *Model) followBackgroundTask(taskID string) tea.Cmd {
	if m.route.watchFollowPending {
		return nil
	}

	m.route.watchFollowPending = true

	return m.fetchBackgroundTask(taskID, true)
}

func (m *Model) fetchBackgroundTask(taskID string, follow bool) tea.Cmd {
	fetch := func() tea.Msg {
		inspection, err := m.controller.InspectBackgroundTask(m.ctx, taskID)

		return backgroundInspectResultMsg{
			taskID: taskID, inspection: inspection, err: err, follow: follow,
		}
	}

	return fetch
}

// applyBackgroundKillResult reports what the control plane answered. The row
// itself changes from the event stream, so this only tells the reader.
func (m *Model) applyBackgroundKillResult(message backgroundKillResultMsg) tea.Cmd {
	if message.err != nil {
		m.appendNotice("Stopping " + message.taskID + " failed: " + safeDetailText(message.err.Error()))

		return nil
	}

	switch message.outcome {
	case background.OutcomeKilled:
		m.appendNotice("Stopping " + message.taskID + "…")
	case background.OutcomeAlreadyExited:
		m.appendNotice(message.taskID + " had already completed.")
	default:
		m.appendNotice(message.taskID + " was not found.")
	}

	return nil
}

// applyBackgroundInspectResult opens the watch's page, or reports why it is
// unavailable. A follow only ever refreshes the page that asked for it: a read
// that lands after the reader left, or after they opened another watch, is
// dropped rather than reopening a route behind them, and a watch that vanished
// between reads stops being followed instead of reporting an error nobody asked
// for.
func (m *Model) applyBackgroundInspectResult(message backgroundInspectResultMsg) tea.Cmd {
	if message.follow {
		m.route.watchFollowPending = false

		if !m.watchPageOpen(message.taskID) {
			return nil
		}
		if message.err != nil {
			m.route.toolDetail.watchRunning = false

			return nil
		}

		m.replaceWatchPage(newBackgroundTaskDetailView(message.inspection))

		return nil
	}
	if message.err != nil {
		m.appendNotice("Watch details unavailable: " + safeDetailText(message.err.Error()))

		return nil
	}

	return m.openToolDetailRoute(newBackgroundTaskDetailView(message.inspection))
}

// watchPageOpen reports whether the detail route is showing this watch.
func (m *Model) watchPageOpen(taskID string) bool {
	return m.route.kind == routeToolDetail && m.route.toolDetail != nil &&
		m.route.toolDetail.watchTaskID == taskID
}

// replaceWatchPage swaps the open watch page's rows for a fresh read. A reader
// holding the tail keeps it; a reader who scrolled up keeps their place.
func (m *Model) replaceWatchPage(view toolDetailView) {
	maximum := m.toolDetailMaximumOffset()
	holding := m.route.offset >= maximum
	m.route.toolDetail = &view

	if holding {
		m.route.offset = m.toolDetailMaximumOffset()

		return
	}

	m.route.offset = min(m.route.offset, m.toolDetailMaximumOffset())
}

// newBackgroundTaskDetailView renders one watch's page: its identity and state,
// the command it runs, and the tail of its own output. The shape follows the
// reference's background-task viewer, which shows the task's description and its
// shell command above the streamed output; the reported task id and running flag
// are what let the route follow a live watch.
func newBackgroundTaskDetailView(inspection coding.BackgroundTaskInspection) toolDetailView {
	task := inspection.Lifecycle()
	state := subagentToolState(backgroundTaskState(task))

	view := toolDetailView{
		title: "Watch details", callIDs: nonEmpty(task.ID),
		watchTaskID: task.ID, watchRunning: !task.Completed,
	}
	view.rows = append(view.rows, detailHeadingRow(
		toolActivityGlyph(state), backgroundTaskRowLabel(task), "", state,
	))
	view.rows = append(view.rows, detailNoteRow(strings.Join(nonEmpty(
		task.ID, backgroundTaskStatusText(task), backgroundTaskElapsedText(task, time.Now()),
	), " · ")))
	view.rows = append(view.rows, watchCommandRows(task.Command)...)

	if output := strings.TrimSpace(sanitizeToolText(inspection.Output)); output != "" {
		view.rows = append(view.rows, detailSection(detailOutputLabel))
		view.rows = append(view.rows, detailCodeRows(
			"", truncateText(output, maximumToolDetailBytes), detailToneBody, 1,
		)...)
	}

	return view
}

// watchCommandRows renders the command a watch runs the way the shell detail
// surface renders a script: a `$ ` prompt on its first row and a hanging indent
// under it, so a multi-line watch reads as one command.
func watchCommandRows(command string) []toolDetailRow {
	script := truncateText(sanitizeToolText(command), maximumToolDetailBytes)
	if script == "" {
		return nil
	}

	lines := strings.Split(script, "\n")
	rows := make([]toolDetailRow, 0, len(lines))
	for index, line := range lines {
		key := ""
		if index == 0 {
			key = detailShellPrompt
		}
		rows = append(rows, toolDetailRow{tone: detailToneCode, indent: 1, key: key, text: line})
	}

	return rows
}
