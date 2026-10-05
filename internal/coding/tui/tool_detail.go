//nolint:wsl_v5 // The detail builder keeps each section's rows adjacent to its guard.
package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/rsbin1178/pips/internal/coding/tools"
)

const (
	// maximumToolDetailRows bounds the logical rows of one detail document;
	// maximumToolDetailBytes bounds its text.
	maximumToolDetailRows = 512
	detailTitleDefault    = "Tool details"
	detailHintScrollFull  = "↑/↓ PgUp/PgDn scroll"
	detailRedacted        = "[redacted]"
	detailShellPrompt     = "$ "
	detailResultLabel     = "Result"
	detailOutputLabel     = "Output"
	detailErrorLabel      = "Error"
	detailEmptyBody       = "No Tool activity is available."
)

type toolDetailView struct {
	title   string
	callIDs []string
	rows    []toolDetailRow
}

// text renders the plain, unwrapped detail body. Tests and NO_COLOR parity
// checks use it; the route wraps and colors the same logical rows.
func (d toolDetailView) text() string {
	lines := make([]string, 0, len(d.rows))
	for _, row := range d.rows {
		lines = append(lines, row.line())
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func (m *Model) openToolDetailRoute(detail toolDetailView) tea.Cmd {
	return m.requestRouteOpen(routeOpenRequest{kind: routeToolDetail, toolDetail: &detail})
}

func (m *Model) updateToolDetailRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	if key == keyCtrlT || key == keyEscape || key == keyCtrlC {
		return m, m.closeRouteToParent()
	}

	visible := m.toolDetailBodyHeight()
	maximum := max(0, m.toolDetailRowCount()-visible)
	switch key {
	case "up", "k":
		m.route.offset = max(0, m.route.offset-1)
	case keyDown, "j":
		m.route.offset = min(maximum, m.route.offset+1)
	case "pgup":
		m.route.offset = max(0, m.route.offset-visible)
	case "pgdown":
		m.route.offset = min(maximum, m.route.offset+visible)
	case "home":
		m.route.offset = 0
	case "end":
		m.route.offset = maximum
	}
	m.route.offset = min(m.route.offset, m.toolDetailMaximumOffset())

	return m, nil
}

func (m *Model) toolDetailRouteContent() string {
	if m.route.toolDetail == nil {
		return "Tool details\n\n" + detailEmptyBody
	}

	rows, _ := m.toolDetailRows(max(1, m.width))

	return strings.TrimRight(strings.Join(rows, "\n"), "\n")
}

func (m *Model) toolDetailRouteView() tea.View {
	width := max(1, m.width)
	height := max(1, m.height)
	rows, overflow := m.toolDetailRows(width)
	bodyHeight := m.toolDetailBodyHeight()
	status := m.toolDetailStatusLine(overflow)
	footer := m.sessionPickerSeparator(width) + "\n" + status

	// A terminal too short for both keeps the footer: the hint explains the
	// keys, while the body is recoverable by resizing.
	content := status
	if height > 2 {
		content = padHeight(m.toolDetailBody(rows, bodyHeight), bodyHeight) + "\n" + footer
	}
	view := m.presentationView(truncateHeight(content, height))

	return view
}

func (m *Model) toolDetailBody(rows []string, bodyHeight int) string {
	if len(rows) == 0 || bodyHeight <= 0 {
		return ""
	}
	maximum := m.toolDetailMaximumOffset()
	start := min(max(0, m.route.offset), maximum)
	end := min(len(rows), start+bodyHeight)

	return strings.Join(rows[start:end], "\n")
}

func (m *Model) toolDetailMaximumOffset() int {
	visible := m.toolDetailBodyHeight()
	total := m.toolDetailRowCount()

	return max(0, total-visible)
}

// toolDetailBodyHeight reserves the pinned footer (separator + status) and
// never returns a height that would push the footer off screen.
func (m *Model) toolDetailBodyHeight() int {
	return max(0, m.height-2)
}

func (m *Model) toolDetailRowCount() int {
	if m.route.toolDetail == nil {
		return 0
	}

	rows, _ := m.toolDetailRows(max(1, m.width))

	return len(rows)
}

func (m *Model) toolDetailRows(width int) ([]string, bool) {
	if m.route.toolDetail == nil {
		return []string{detailTitleDefault, "", detailEmptyBody}, false
	}

	rows := detailPhysicalRows(m.route.toolDetail.rows, width, m.theme, m.options.NoColor)
	if len(rows) == 0 {
		rows = []string{detailTitleDefault, "", detailEmptyBody}
	}
	rows = clampDetailPhysicalRows(rows)
	overflow := len(rows) > m.toolDetailBodyHeight()

	return rows, overflow
}

func (m *Model) toolDetailStatusLine(overflow bool) string {
	title := detailTitleDefault
	if m.route.toolDetail != nil && m.route.toolDetail.title != "" {
		title = m.route.toolDetail.title
	}

	position := ""
	if overflow {
		position = m.toolDetailPositionLabel()
	}
	// The footer names the surface and always keeps a key hint: each step down
	// the ladder drops or shortens one part instead of cutting the line.
	value := title
	for _, candidate := range detailStatusCandidates(title, position) {
		if ansi.StringWidth(candidate) <= m.width {
			value = candidate

			break
		}
	}
	value = ansi.Truncate(value, max(1, m.width), "…")
	if m.options.NoColor {
		return value
	}

	return lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(value)
}

// detailStatusCandidates orders footer variants from most to least
// informative so a narrow terminal degrades instead of truncating.
func detailStatusCandidates(title, position string) []string {
	hints := []string{
		detailHintScrollFull + " · Ctrl+T/Esc close",
		"↑/↓ scroll · Ctrl+T/Esc close",
		"↑/↓ scroll · esc close",
		"↑/↓ · esc close",
		"esc close",
	}
	candidates := make([]string, 0, len(hints)*2+1)
	for _, hint := range hints {
		if position != "" {
			candidates = append(candidates, title+" · "+position+" · "+hint)
		}
		candidates = append(candidates, title+" · "+hint)
	}
	candidates = append(candidates, title)

	return candidates
}

func (m *Model) toolDetailPositionLabel() string {
	visible := m.toolDetailBodyHeight()
	total := m.toolDetailRowCount()
	start := min(max(0, m.route.offset), m.toolDetailMaximumOffset())
	end := min(total, start+visible)

	return fmt.Sprintf("%d-%d/%d", start+1, end, total)
}

func newToolDetailView(block timelineBlock) toolDetailView {
	view := toolDetailView{title: toolDetailTitle(block)}
	rows := make([]toolDetailRow, 0, len(block.tools)*8)
	budget := newDetailByteBudget(maximumToolDetailBytes)
	// Progress and the result share the tail of the document: capping the
	// argument tree at half guarantees both a non-zero share.
	if budget.remaining > 0 {
		budget.remaining /= 2
	}

	for index, activity := range block.tools {
		if activity.id != "" {
			view.callIDs = append(view.callIDs, activity.id)
		}
		if budget.exhausted() {
			rows = append(rows, detailTruncationRow(0))

			break
		}
		if index > 0 {
			rows = append(rows, toolDetailRow{})
		}
		rows = append(rows, toolDetailCallRows(index+1, activity, budget)...)
	}
	// The section budgets already reserve room for every part; the clamp is the
	// document-wide backstop measured against the same byte limit.
	view.rows = clampDetailRows(rows, maximumToolDetailBytes)

	return view
}

// clampDetailPhysicalRows bounds the rendered rows after wrapping, so one
// oversized value cannot make the route arbitrarily expensive to draw.
func clampDetailPhysicalRows(rows []string) []string {
	if len(rows) <= maximumToolDetailRows {
		return rows
	}

	visible := rows[:maximumToolDetailRows-1]

	return append(visible, "… more rows omitted …")
}

// clampDetailRows enforces the detail document's byte and row bounds over the
// assembled rows, so one oversized value cannot produce an unbounded route.
func clampDetailRows(rows []toolDetailRow, remainingBytes int) []toolDetailRow {
	if len(rows) == 0 {
		return rows
	}

	used := 0
	visible := 0
	for ; visible < len(rows); visible++ {
		row := rows[visible]
		used += len(row.key) + len(row.text) + 1
		if used > remainingBytes || visible >= maximumToolDetailRows {
			break
		}
	}
	if visible >= len(rows) {
		return rows
	}

	if visible == 0 {
		// Always keep the call heading so the route still names what it shows.
		visible = 1
	}
	result := append(slices.Clone(rows[:visible]), detailTruncationRow(0))

	return result
}

func toolDetailTitle(block timelineBlock) string {
	if len(block.tools) == 0 {
		return detailTitleDefault
	}

	switch block.tools[0].class {
	case toolClassExplore:
		return "Exploration details"
	case toolClassShell:
		return "Shell details"
	case toolClassPatch:
		return "Workspace update details"
	case toolClassSubagent:
		return "Agent activity details"
	case toolClassTaskList:
		return "Plan details"
	case toolClassGeneric, toolClassPlan:
		return "Tool call details"
	default:
		return detailTitleDefault
	}
}

// toolDetailCallRows renders one call: heading, facts, arguments/changes,
// progress, and the result or error body.
func toolDetailCallRows(index int, activity toolActivity, budget *detailByteBudget) []toolDetailRow {
	rows := make([]toolDetailRow, 0, 12)
	rows = append(rows, detailHeadingRowFor(index, activity))
	budget.spend(len(activity.subject) + len(activity.update))
	if facts := toolResultFacts(activity); facts != "" {
		rows = append(rows, toolDetailRow{tone: detailToneMuted, indent: 1, text: facts})
	}
	if activity.class == toolClassTaskList {
		rows = append(rows, taskListDetailRows(activity)...)

		return rows
	}
	rows = append(rows, toolDetailArgumentRows(activity, budget)...)

	if changes := patchDetailChanges(activity); changes != "" {
		rows = append(rows, detailSection("Changes"))
		rows = append(rows, detailDiffRows(changes)...)
	}

	if update := truncateText(sanitizeToolText(activity.update), budget.child(0).remainingBytes()); update != "" {
		rows = append(rows, detailSection("Progress"))
		rows = append(rows, detailCodeRows("", update, detailToneBody, 1)...)
	}

	if body, label := toolDetailBodyLabel(activity); body != "" &&
		!detailResultRepeatsChanges(activity, body) {
		rows = append(rows, detailSection(label))
		rows = append(rows, toolDetailResultRows(activity, body, budget)...)
	}

	return rows
}

// detailHeadingRowFor names one call the way the expanded timeline rows do, so
// the detail route reads as the same projection at more depth.
// taskListDetailRows renders the decoded plan as a checklist plus its
// explanation, instead of the raw argument tree and progress JSON.
func taskListDetailRows(activity toolActivity) []toolDetailRow {
	update, err := tasklist.Decode(activity.arguments)
	if err != nil {
		rows := []toolDetailRow{detailSection("Arguments")}
		rows = append(rows, detailNoteRow("[invalid arguments omitted]"))

		return rows
	}
	if explanation := oneLineToolText(update.Explanation); explanation != "" {
		rows := []toolDetailRow{detailSection("Explanation")}
		rows = append(rows, detailNoteRow(explanation))
		rows = append(rows, taskListChecklistRows(update)...)

		return rows
	}

	return taskListChecklistRows(update)
}

func taskListChecklistRows(update tasklist.Update) []toolDetailRow {
	rows := []toolDetailRow{detailSection("Plan")}
	for _, item := range update.Plan {
		rows = append(rows, toolDetailRow{
			tone: detailToneTask(item.Status), indent: 1,
			text: taskListGlyph(item.Status) + " " + oneLineToolText(item.Step),
		})
	}

	return rows
}

func detailToneTask(status tasklist.Status) toolDetailTone {
	switch status {
	case tasklist.StatusCompleted:
		return detailToneAdded
	case tasklist.StatusInProgress:
		return detailToneHeading
	case tasklist.StatusPending, "":
		return detailToneMuted
	default:
		return detailToneBody
	}
}

// detailHeadingRowFor names one call the way the expanded timeline rows do, so
// the detail route reads as the same projection at more depth.
func detailHeadingRowFor(index int, activity toolActivity) toolDetailRow {
	glyph := toolActivityGlyph(activity.state)
	label := expandedToolCallLabel(activity)
	switch activity.class {
	case toolClassPatch, toolClassShell, toolClassTaskList:
		_, verb, subject := toolActivityHeading(activity.class, activity.state, []toolActivity{activity})
		label = strings.TrimSpace(verb + " " + subject)
	case toolClassExplore, toolClassGeneric, toolClassPlan, toolClassSubagent:
	}
	if label == "" {
		label = activity.name
	}
	if index > 1 {
		label = fmt.Sprintf("%d. %s", index, label)
	}

	return detailHeadingRow(glyph, label, "", activity.state)
}

// toolDetailArgumentRows renders the argument tree against a capped share of
// the document budget, so the Result/Error body always has room of its own.
func toolDetailArgumentRows(activity toolActivity, budget *detailByteBudget) []toolDetailRow {
	fields, ok := detailArgumentFields(activity)
	if !ok {
		rows := []toolDetailRow{detailSection("Arguments")}
		rows = append(rows, detailNoteRow("[invalid arguments omitted]"))

		return rows
	}
	if activity.class == toolClassShell {
		if command, rest, found := detailTakeField(fields, "command"); found {
			rows := []toolDetailRow{detailSection("Command")}
			rows = append(rows, detailCommandRows(command, budget)...)
			rows = append(rows, detailFieldRows(rest, budget)...)

			return rows
		}
	}
	if activity.class == toolClassPatch {
		// A parsed diff already carries the patch text, so the raw field would
		// only add an escaped duplicate. An unparsed patch stays visible.
		if _, parsed := parsePatchDisplayChanges(activity); parsed {
			_, fields, _ = detailTakeField(fields, "patch")
		}
	}
	if len(fields) == 0 {
		return nil
	}

	rows := []toolDetailRow{detailSection("Arguments")}
	rows = append(rows, detailObjectRows("", fields, 1, budget.child(budget.remainingBytes()/2))...)

	return rows
}

// detailArgumentFields decodes the call arguments into redacted, ordered
// fields. A non-object or malformed payload reports false so callers degrade to
// identity instead of failing.
func detailArgumentFields(activity toolActivity) ([]jsonTreeField, bool) {
	if len(activity.arguments) == 0 {
		return nil, true
	}

	value, ok := decodeJSONTree(activity.arguments)
	if !ok || value.kind != jsonTreeObject {
		return nil, false
	}

	return detailRedactFields(value.fields), true
}

func detailRedactFields(fields []jsonTreeField) []jsonTreeField {
	result := make([]jsonTreeField, 0, len(fields))
	for _, field := range fields {
		if isSensitiveToolKey(field.key) {
			result = append(result, jsonTreeField{
				key:   field.key,
				value: jsonTreeValue{kind: jsonTreeScalar, scalar: detailRedacted},
			})

			continue
		}
		result = append(result, jsonTreeField{key: field.key, value: detailRedactTree(field.value)})
	}

	return result
}

func detailTakeField(fields []jsonTreeField, key string) (jsonTreeValue, []jsonTreeField, bool) {
	for index, field := range fields {
		if field.key != key {
			continue
		}
		rest := make([]jsonTreeField, 0, len(fields)-1)
		rest = append(rest, fields[:index]...)
		rest = append(rest, fields[index+1:]...)

		return field.value, rest, true
	}

	return jsonTreeValue{}, fields, false
}

func detailRedactTree(value jsonTreeValue) jsonTreeValue {
	switch value.kind {
	case jsonTreeScalar:
		if containsSensitiveToolText(value.scalar) {
			return jsonTreeValue{kind: jsonTreeScalar, scalar: detailRedacted}
		}

		return value
	case jsonTreeList:
		result := jsonTreeValue{kind: jsonTreeList, list: make([]jsonTreeValue, 0, len(value.list))}
		for _, child := range value.list {
			result.list = append(result.list, detailRedactTree(child))
		}

		return result
	case jsonTreeObject:
		return jsonTreeValue{kind: jsonTreeObject, fields: detailRedactFields(value.fields)}
	case jsonTreeOmitted:
		return value
	default:
		return value
	}
}

func detailCommandRows(command jsonTreeValue, budget *detailByteBudget) []toolDetailRow {
	script := shellCommandScript(command.scalar)
	if containsSensitiveToolText(script) {
		return []toolDetailRow{detailNoteRow("[sensitive command omitted]")}
	}

	script = truncateText(sanitizeToolText(script), budget.remainingBytes())
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

func detailFieldRows(fields []jsonTreeField, budget *detailByteBudget) []toolDetailRow {
	if len(fields) == 0 {
		return nil
	}

	rows := []toolDetailRow{detailSection("Arguments")}
	rows = append(rows, detailObjectRows("", fields, 1, budget.child(budget.remainingBytes()/2))...)

	return rows
}

// detailResultRepeatsChanges reports whether a parsed patch result is only the
// file list the Changes section already renders.
func detailResultRepeatsChanges(activity toolActivity, body string) bool {
	if activity.class != toolClassPatch {
		return false
	}
	if _, parsed := parsePatchDisplayChanges(activity); !parsed {
		return false
	}

	for line := range strings.SplitSeq(strings.TrimSpace(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) < 2 || (trimmed[0] != 'A' && trimmed[0] != 'M' && trimmed[0] != 'D') ||
			trimmed[1] != ' ' {
			return false
		}
	}

	return true
}

func toolDetailBodyLabel(activity toolActivity) (body, label string) {
	if activity.body == "" {
		return "", ""
	}

	switch activity.class {
	case toolClassShell:
		label = detailOutputLabel
	case toolClassSubagent:
		label = detailResultLabel
	default:
		label = detailResultLabel
	}
	if activity.state == toolStateFailed || activity.state == toolStateInterrupted {
		label = detailErrorLabel
	}

	return activity.body, label
}

func toolDetailResultRows(activity toolActivity, body string, budget *detailByteBudget) []toolDetailRow {
	if containsSensitiveToolText(body) {
		return []toolDetailRow{detailNoteRow("[sensitive result omitted]")}
	}

	body = truncateText(sanitizeToolText(body), budget.remainingBytes())
	budget.spend(len(body))
	if body == "" {
		return nil
	}
	if activity.class == toolClassShell && activity.hasHeader {
		return detailShellStreamRows(body)
	}
	if activity.class == toolClassGeneric {
		if value, ok := decodeJSONTree([]byte(body)); ok {
			return detailTreeRows("", value, 1, budget)
		}
	}

	return detailCodeRows("", body, detailToneBody, 1)
}

// detailShellStreamRows labels the `stdout:` / `stderr:` markers a shell
// result body uses so the two streams read as distinct sections.
func detailShellStreamRows(body string) []toolDetailRow {
	rows := make([]toolDetailRow, 0, 8)
	for line := range strings.SplitSeq(body, "\n") {
		tone := detailToneBody
		switch strings.TrimSpace(line) {
		case "stdout:", "stderr:":
			tone = detailToneKey
		default:
		}

		rows = append(rows, toolDetailRow{tone: tone, indent: 1, text: line})
	}

	return rows
}

func detailDiffRows(changes string) []toolDetailRow {
	rows := make([]toolDetailRow, 0, 16)
	for line := range strings.SplitSeq(changes, "\n") {
		rows = append(rows, toolDetailRow{
			tone: detailDiffTone(line), indent: 1, text: strings.TrimRight(line, " "),
		})
	}

	return rows
}

func detailDiffTone(line string) toolDetailTone {
	trimmed := strings.TrimLeft(line, " ")
	switch {
	case strings.HasPrefix(trimmed, "+"):
		return detailToneAdded
	case strings.HasPrefix(trimmed, "-"):
		return detailToneRemoved
	case len(trimmed) > 1 && (trimmed[0] == 'A' || trimmed[0] == 'M' || trimmed[0] == 'D'):
		return detailToneFile
	default:
		return detailToneBody
	}
}

// toolResultFacts summarizes a built-in result header for the detail heading.
// Counts the heading already states are left out rather than repeated.
func toolResultFacts(activity toolActivity) string {
	if !activity.hasHeader {
		return ""
	}

	facts := make([]string, 0, 8)
	appendCount := func(value int, label string) {
		if value > 0 {
			facts = append(facts, fmt.Sprintf("%d %s", value, label))
		}
	}
	appendCount(activity.header.Counts.Lines, "lines")
	appendCount(activity.header.Counts.Entries, "entries")
	if activity.class != toolClassPatch {
		appendCount(activity.header.Counts.Files, "files")
	}
	appendCount(activity.header.Counts.Matches, "matches")
	appendCount(activity.header.Counts.Scanned, "scanned")
	appendCount(activity.header.Counts.Skipped, "skipped")
	if activity.header.Truncated {
		facts = append(facts, "truncated")
	}
	if execution := activity.header.Execution; execution != nil {
		facts = append(facts, shellExecutionFacts(*execution)...)
	}

	return strings.Join(facts, " · ")
}

// shellExecutionFacts renders the process outcome without repeating a status
// that the exit code already conveys.
func shellExecutionFacts(execution tools.ResultExecution) []string {
	facts := make([]string, 0, 3)
	if execution.ExitCode != nil {
		facts = append(facts, fmt.Sprintf("exit %d", *execution.ExitCode))
	} else if execution.Status != "" && execution.Status != "exited" {
		facts = append(facts, strings.ReplaceAll(execution.Status, "_", " "))
	}
	if execution.DurationMS > 0 {
		facts = append(facts, formatInteractionDuration(execution.DurationMS))
	}

	return facts
}

// toolDetailResult renders a bounded, redacted result body for detail rows.
func toolDetailResult(activity toolActivity) string {
	if containsSensitiveToolText(activity.body) {
		return "[sensitive result omitted]"
	}

	return truncateText(sanitizeToolText(activity.body), maximumToolDetailBytes)
}

func padHeight(content string, height int) string {
	if height <= 0 {
		return ""
	}
	if padding := height - lipgloss.Height(content); padding > 0 {
		return content + strings.Repeat("\n", padding)
	}

	return truncateHeight(content, height)
}
