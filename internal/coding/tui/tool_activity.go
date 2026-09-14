//nolint:wsl_v5 // Semantic projection keeps each fail-soft decode next to its fallback.
package tui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	codingtools "github.com/rsbin1178/pips/internal/coding/tools"
)

const (
	compactToolPreviewLines = 4
	compactExploreRows      = 6
	maximumExpandedToolRows = 256
	maximumToolDetailBytes  = 16 << 10
	toolNameRead            = "read"
	toolNameList            = "ls"
	toolNameGlob            = "glob"
	toolNameSearch          = "grep"
	toolNameShell           = "shell"
	toolNamePatch           = "apply_patch"
	toolStateTextFailed     = "failed"
)

type toolActivityClass uint8

const (
	toolClassGeneric toolActivityClass = iota
	toolClassExplore
	toolClassShell
	toolClassPatch
	toolClassPlan
	toolClassSubagent
)

type toolActivityState uint8

const (
	toolStateRunning toolActivityState = iota
	toolStateSucceeded
	toolStateFailed
	toolStateInterrupted
)

type toolActivity struct {
	id             string
	name           string
	runID          string
	childSessionID string
	turn           int
	position       int
	order          int
	class          toolActivityClass
	state          toolActivityState
	action         string
	subject        string
	invocation     string
	arguments      ai.JSON
	update         string
	result         string
	body           string
	header         codingtools.ResultHeader
	hasHeader      bool
	preview        []string
}

type toolActivityRecord struct {
	call           coding.ToolCall
	runID          string
	turn           int
	position       int
	resultPosition int
	order          int
	live           bool
	status         coding.ToolStatus
	update         []ai.Part
	result         ai.ToolResultPart
	hasResult      bool
}

type durableSubagentArgs struct {
	Role subagent.Role `json:"role"`
	Task string        `json:"task"`
}

type durableSubagentResult struct {
	Schema         string           `json:"schema"`
	Role           subagent.Role    `json:"role"`
	ChildSessionID string           `json:"child_session_id"`
	Outcome        subagent.Outcome `json:"outcome"`
	Code           string           `json:"code"`
	Turns          int              `json:"turns"`
	ToolCalls      int              `json:"tool_calls"`
	Usage          ai.Usage         `json:"usage"`
	DurationMillis int64            `json:"duration_millis"`
}

// projectToolActivities reconstructs completed calls from the durable
// transcript and overlays live ToolState by call ID. The transcript remains
// the replay source of truth; live state only adds status/progress that is not
// durable yet.
//
//nolint:gocyclo // Transcript reconstruction and live overlay are one ordered merge.
func projectToolActivities(
	state coding.State,
	excluded map[string]struct{},
) []toolActivity {
	records := make(map[string]*toolActivityRecord)
	ordered := make([]*toolActivityRecord, 0, len(state.Tools))

	recordFor := func(id string) *toolActivityRecord {
		if id == "" {
			return nil
		}
		if _, skip := excluded[id]; skip {
			return nil
		}
		if record, ok := records[id]; ok {
			return record
		}

		record := &toolActivityRecord{order: len(ordered)}
		records[id] = record
		ordered = append(ordered, record)

		return record
	}

	for messageIndex, message := range state.Transcript {
		position := messageIndex + 1
		for _, part := range portableMessageParts(message) {
			switch value := part.(type) {
			case ai.ToolCallPart:
				record := recordFor(value.ID)
				if record == nil {
					continue
				}
				record.call = coding.ToolCall{
					ID: value.ID, Name: value.Name, Arguments: value.Args,
				}
				if record.position == 0 {
					record.position = position
				}
			case ai.ToolResultPart:
				record := recordFor(value.ToolCallID)
				if record == nil {
					continue
				}
				if record.call.ID == "" {
					record.call.ID = value.ToolCallID
				}
				if record.call.Name == "" {
					record.call.Name = value.Name
				}
				record.result = value
				record.hasResult = true
				record.position = position
				record.resultPosition = position
			}
		}
	}

	for _, live := range state.Tools {
		record := recordFor(live.Call.ID)
		if record == nil {
			continue
		}

		record.call = live.Call
		record.runID = live.RunID
		record.turn = live.Turn
		record.live = true
		record.status = live.Status
		record.update = live.Update
		if result, ok := toolResultPart(live.Result, live.Call.ID); ok {
			record.result = result
			record.hasResult = true
			if record.resultPosition == 0 {
				record.position = len(state.Transcript) + 1
			}
		}
		if record.position == 0 {
			record.position = len(state.Transcript) + 1
		}
	}

	activities := make([]toolActivity, 0, len(ordered))
	for _, record := range ordered {
		// A transcript ToolCall without a result is not yet stable and may be
		// followed immediately by ToolStarted. Wait for live state so the call
		// cannot be committed prematurely between those two events.
		if !record.live && !record.hasResult {
			continue
		}
		if record.call.ID == "" || record.call.Name == "" {
			continue
		}

		activities = append(activities, describeToolActivity(*record))
	}

	sort.SliceStable(activities, func(left, right int) bool {
		if activities[left].position != activities[right].position {
			return activities[left].position < activities[right].position
		}

		return activities[left].order < activities[right].order
	})

	return activities
}

func toolResultPart(message ai.ToolMessage, callID string) (ai.ToolResultPart, bool) {
	for _, result := range message.Parts {
		if callID == "" || result.ToolCallID == callID {
			return result, true
		}
	}

	return ai.ToolResultPart{}, false
}

func describeToolActivity(record toolActivityRecord) toolActivity {
	activity := toolActivity{
		id: record.call.ID, name: oneLineToolText(record.call.Name),
		runID: record.runID, turn: record.turn,
		position: record.position, order: record.order,
		arguments: record.call.Arguments,
		update:    visibleToolParts(record.update),
	}
	activity.class, activity.action, activity.subject = describeToolCall(record.call)
	activity.invocation = compactToolInvocation(record.call)
	activity.state = classifyToolActivity(record)

	if record.hasResult {
		activity.result = visibleToolParts(record.result.Content)
		activity.body = activity.result
		if header, body, err := codingtools.ParseResult(activity.result); err == nil &&
			header.Tool == record.call.Name {
			activity.header = header
			activity.hasHeader = true
			activity.body = body
		}
	}

	activity.preview = compactToolPreview(activity)

	return activity
}

func classifyToolActivity(record toolActivityRecord) toolActivityState {
	if record.live && record.status == coding.ToolStatusRunning {
		return toolStateRunning
	}
	if !record.hasResult {
		return toolStateInterrupted
	}

	text := visibleToolParts(record.result.Content)
	header, _, err := codingtools.ParseResult(text)
	if err == nil && !header.OK {
		if header.Code == "canceled" || header.Code == "deadline_exceeded" {
			return toolStateInterrupted
		}

		return toolStateFailed
	}
	if record.result.IsError {
		return toolStateFailed
	}

	return toolStateSucceeded
}

func describeToolCall(call coding.ToolCall) (toolActivityClass, string, string) {
	arguments := decodeToolArguments(call.Arguments)

	switch call.Name {
	case toolNameRead:
		return toolClassExplore, "Read", safeWorkspaceToolPath(
			toolArgumentString(arguments, "path"),
		)
	case toolNameList:
		workspacePath := safeWorkspaceToolPath(toolArgumentString(arguments, "path"))
		if workspacePath == "" {
			workspacePath = "."
		}

		return toolClassExplore, "List", workspacePath
	case toolNameGlob:
		return toolClassExplore, "Glob", subjectInPath(
			toolArgumentString(arguments, "pattern"),
			toolArgumentString(arguments, "path"),
		)
	case toolNameSearch:
		return toolClassExplore, "Search", subjectInPath(
			toolArgumentString(arguments, "pattern"),
			toolArgumentString(arguments, "path"),
		)
	case toolNameShell:
		return toolClassShell, "Run", safeToolSubject(
			toolArgumentString(arguments, "command"),
		)
	case toolNamePatch:
		return toolClassPatch, "Update", workspaceLabel
	case planmode.EnterToolName:
		return toolClassPlan, "Enter plan mode", ""
	case planmode.ExitToolName:
		return toolClassPlan, "Submit for approval", ""
	case subagent.ToolName, subagent.SpawnToolName:
		role := subagent.Role(toolArgumentString(arguments, "role"))

		return toolClassSubagent, subagentActivityLabel(role, subagent.StateRunning),
			oneLineSubagentTask(toolArgumentString(arguments, "task"))
	default:
		return toolClassGeneric, "Call", call.Name
	}
}

func subjectInPath(subject, path string) string {
	subject = oneLineToolText(subject)
	path = safeWorkspaceToolPath(path)
	if path == "" {
		return subject
	}
	if subject == "" {
		return path
	}

	return subject + " in " + path
}

func safeWorkspaceToolPath(value string) string {
	value = oneLineToolText(value)
	clean := path.Clean(value)
	if value != "" && (path.IsAbs(value) || filepath.IsAbs(value) ||
		clean == ".." || strings.HasPrefix(clean, "../")) {
		return "[invalid path]"
	}

	return value
}

func decodeToolArguments(value ai.JSON) map[string]any {
	arguments := make(map[string]any)
	if len(value) == 0 || json.Unmarshal(value, &arguments) != nil {
		return map[string]any{}
	}

	return arguments
}

func toolArgumentString(arguments map[string]any, key string) string {
	value, ok := arguments[key].(string)
	if !ok {
		return ""
	}

	return oneLineToolText(value)
}

func compactToolInvocation(call coding.ToolCall) string {
	name := oneLineToolText(call.Name)
	if name == "" {
		name = "tool"
	}

	arguments := decodeToolArguments(call.Arguments)
	fields := make(map[string]string)
	for _, key := range []string{"query", "path", "pattern", "url"} {
		value := toolArgumentString(arguments, key)
		if value == "" || isSensitiveToolKey(key) || containsSensitiveToolText(value) {
			continue
		}
		if key == "path" && safeWorkspaceToolPath(value) != value {
			continue
		}
		if key == "url" {
			value = safeCompactURL(value)
			if value == "" {
				continue
			}
		}
		fields[key] = truncateText(value, 160)
	}
	if len(fields) == 0 {
		return name
	}

	data, err := json.Marshal(fields)
	if err != nil {
		return name
	}

	return name + "(" + string(data) + ")"
}

func safeCompactURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}

	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed.String()
}

func compactToolPreview(activity toolActivity) []string {
	if activity.class != toolClassPatch && containsSensitiveToolText(activity.body) {
		return nil
	}

	switch activity.class {
	case toolClassExplore:
		return nil
	case toolClassShell:
		if !activity.hasHeader {
			return nil
		}
		if activity.state == toolStateFailed {
			return boundedFailureToolLines(activity.body, compactToolPreviewLines)
		}

		return boundedToolLines(activity.body, compactToolPreviewLines)
	case toolClassPatch:
		if !activity.hasHeader {
			return nil
		}
		if preview, ok := compactPatchDiffPreview(activity); ok {
			return preview
		}

		return patchResultLines(activity.body, compactToolPreviewLines)
	case toolClassGeneric, toolClassSubagent:
		lines := boundedToolLines(activity.body, 2)
		if len(lines) > 0 {
			return lines
		}

		return boundedToolLines(activity.update, 1)
	default:
		return nil
	}
}

func safeToolSubject(value string) string {
	if containsSensitiveToolText(value) {
		return "[sensitive command omitted]"
	}

	return value
}

func boundedToolLines(value string, maximum int) []string {
	value = sanitizeToolText(value)
	if value == "" || maximum <= 0 {
		return nil
	}

	lines := strings.Split(value, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	if len(lines) <= maximum {
		return lines
	}
	if maximum == 1 {
		return []string{lines[0]}
	}
	if maximum == 2 {
		return []string{lines[0], fmt.Sprintf(
			"… +%d lines (ctrl+t for details)", len(lines)-1,
		)}
	}

	head := maximum - 2
	omitted := len(lines) - head - 1
	result := append([]string{}, lines[:head]...)
	result = append(result, fmt.Sprintf("… +%d lines (ctrl+t for details)", omitted))
	result = append(result, lines[len(lines)-1])

	return result
}

//nolint:gocyclo,wsl_v5 // Failure previews preserve one actionable diagnostic within a fixed bound.
func boundedFailureToolLines(value string, maximum int) []string {
	value = sanitizeToolText(value)
	if value == "" || maximum <= 0 {
		return nil
	}

	lines := strings.Split(value, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || len(lines) <= maximum {
		return lines
	}

	diagnostic := -1
	for index, line := range lines {
		lower := strings.ToLower(line)
		for _, marker := range []string{
			"eperm", "eacces", "enoent", "erofs", "mkdir", "prisma", "p1001", "sandbox",
		} {
			if strings.Contains(lower, marker) {
				diagnostic = index
				break
			}
		}
		if diagnostic >= 0 {
			break
		}
	}
	if diagnostic < 0 {
		return boundedToolLines(value, maximum)
	}
	if maximum == 1 {
		return []string{lines[diagnostic]}
	}
	if diagnostic == 0 || diagnostic == len(lines)-1 {
		return boundedToolLines(value, maximum)
	}
	if maximum == 2 {
		return []string{
			lines[diagnostic],
			fmt.Sprintf("… +%d lines (ctrl+t for details)", len(lines)-1),
		}
	}

	result := []string{lines[0], lines[diagnostic]}
	if maximum > 3 {
		result = append(result,
			fmt.Sprintf("… +%d lines (ctrl+t for details)", len(lines)-3),
		)
	}
	result = append(result, lines[len(lines)-1])

	return result
}

func patchResultLines(value string, maximum int) []string {
	lines := boundedToolLines(value, maximum)
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "A ") || strings.HasPrefix(line, "M ") ||
			strings.HasPrefix(line, "D ") || strings.HasPrefix(line, "… ") {
			result = append(result, line)
		}
	}

	return result
}

func sanitizeToolText(value string) string {
	value = ansi.Strip(value)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")

	var sanitized strings.Builder
	sanitized.Grow(len(value))
	for _, char := range value {
		switch {
		case char == '\n':
			sanitized.WriteRune(char)
		case char == '\t':
			sanitized.WriteString("    ")
		case !unicode.IsControl(char):
			sanitized.WriteRune(char)
		}
	}

	return strings.TrimSpace(sanitized.String())
}

func oneLineToolText(value string) string {
	return strings.Join(strings.Fields(sanitizeToolText(value)), " ")
}

func isSensitiveToolKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	if slices.Contains([]string{
		"authorization", "cookie", "cookies", "key", "credential", "credentials",
		"password", "passwd", "secret", "secrets", "token", "tokens", "headers",
	}, key) {
		return true
	}

	return strings.Contains(key, "api_key") || strings.Contains(key, "apikey") ||
		strings.HasSuffix(key, "_token") || strings.HasSuffix(key, "_secret") ||
		strings.HasSuffix(key, "_password") || strings.HasSuffix(key, "_key")
}

func containsSensitiveToolText(value string) bool {
	value = strings.ToLower(value)
	for _, marker := range []string{
		"authorization", "api_key", "apikey", "bearer ", "cookie", "credential",
		"password", "passwd", "secret", "access_token", "refresh_token", "private_key",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}

	return false
}

func renderToolActivityBlock(
	block timelineBlock,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	return renderToolActivityBlockWithLimit(
		block,
		width,
		theme,
		noColor,
		compactExploreRows,
	)
}

type expandedToolRow struct {
	prefix string
	text   string
	state  toolActivityState
	output bool
}

func renderExpandedToolActivityBlock(
	block timelineBlock,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	if len(block.tools) == 0 {
		return ""
	}
	width = max(1, width)

	state := combinedToolState(block.tools)
	class := block.tools[0].class
	glyph, verb, subject := toolActivityHeading(class, state, block.tools)
	lines := []string{renderToolHeading(glyph, verb, subject, state, width, theme, noColor)}
	for _, row := range expandedToolActivityRows(class, block.tools, width) {
		lines = append(lines, renderExpandedToolRow(row, width, theme, noColor))
	}

	return strings.Join(lines, "\n")
}

func expandedToolActivityRows(
	class toolActivityClass,
	activities []toolActivity,
	width int,
) []expandedToolRow {
	rows := make([]expandedToolRow, 0, len(activities)*2)

	switch class {
	case toolClassExplore:
		for index, activity := range activities {
			last := index == len(activities)-1
			branch := "  ├ "
			outputBranch := "  │   └ "
			outputContinuation := "  │     "
			if last {
				branch = "  └ "
				outputBranch = "      └ "
				outputContinuation = "        "
			}

			rows = append(rows, expandedToolRow{
				prefix: branch,
				text:   expandedToolCallLabel(activity),
				state:  activity.state,
			})
			appendExpandedToolOutput(
				&rows,
				activity,
				outputBranch,
				outputContinuation,
				width,
			)
		}
	case toolClassGeneric:
		activity := activities[0]
		rows = append(rows, expandedToolRow{
			prefix: "  └ ", text: expandedToolCallLabel(activity), state: activity.state,
		})
		appendExpandedToolOutput(&rows, activity, "      └ ", "        ", width)
	case toolClassPlan, toolClassSubagent:
		activity := activities[0]
		appendExpandedToolOutput(&rows, activity, "  └ ", "    ", width)
	case toolClassShell, toolClassPatch:
		appendExpandedToolOutput(&rows, activities[0], "  └ ", "    ", width)
	}

	return rows
}

func expandedToolCallLabel(activity toolActivity) string {
	label := strings.TrimSpace(activity.action + " " + activity.subject)
	if activity.class == toolClassGeneric {
		label = activity.invocation
	}
	if label == "" {
		label = activity.name
	}
	if activity.state == toolStateFailed || activity.state == toolStateInterrupted {
		label += " · " + toolActivityReason(activity)
	}

	return label
}

func appendExpandedToolOutput(
	rows *[]expandedToolRow,
	activity toolActivity,
	firstPrefix string,
	continuationPrefix string,
	width int,
) {
	output := expandedToolOutput(activity)
	if output == "" {
		return
	}

	first := true
	outputRows := 0
	for logicalLine := range strings.SplitSeq(output, "\n") {
		prefix := continuationPrefix
		if first {
			prefix = firstPrefix
		}
		available := max(1, width-lipgloss.Width(prefix))
		var wrapped string
		if available <= 8 {
			wrapped = ansi.Truncate(logicalLine, available, "…")
		} else {
			wrapped = lipgloss.Wrap(logicalLine, available, "")
		}

		for line := range strings.SplitSeq(wrapped, "\n") {
			if outputRows >= maximumExpandedToolRows {
				*rows = append(*rows, expandedToolRow{
					prefix: continuationPrefix,
					text:   "… output truncated …",
					state:  activity.state,
					output: true,
				})

				return
			}

			prefix = continuationPrefix
			if first {
				prefix = firstPrefix
			}
			*rows = append(*rows, expandedToolRow{
				prefix: prefix,
				text:   line,
				state:  activity.state,
				output: true,
			})
			first = false
			outputRows++
		}
	}
}

func expandedToolOutput(activity toolActivity) string {
	parts := make([]string, 0, 2)
	if facts := toolResultFacts(activity); facts != "" {
		parts = append(parts, facts)
	}
	if result := toolDetailResult(activity); result != "" {
		parts = append(parts, result)
	} else if update := truncateText(
		sanitizeToolText(activity.update), maximumToolDetailBytes,
	); update != "" {
		parts = append(parts, update)
	}

	return strings.Join(parts, "\n")
}

func renderExpandedToolRow(
	row expandedToolRow,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	line := ansi.Truncate(row.prefix+row.text, max(1, width), "…")
	if noColor {
		return line
	}

	palette := paletteFor(theme)
	tone := palette.idle
	if row.output {
		tone = palette.muted
	}
	switch row.state {
	case toolStateRunning:
		if !row.output {
			tone = palette.model
		}
	case toolStateFailed:
		tone = palette.error
	case toolStateInterrupted:
		tone = palette.warning
	case toolStateSucceeded:
	}

	return lipgloss.NewStyle().Foreground(tone).Render(line)
}

func renderToolActivityBlockWithLimit(
	block timelineBlock,
	width int,
	theme colorTheme,
	noColor bool,
	exploreLimit int,
) string {
	if len(block.tools) == 0 {
		return ""
	}
	width = max(1, width)

	state := combinedToolState(block.tools)
	class := block.tools[0].class
	glyph, verb, subject := toolActivityHeading(class, state, block.tools)
	heading := renderToolHeading(glyph, verb, subject, state, width, theme, noColor)
	rows := toolActivityRows(class, block.tools, exploreLimit)
	if len(rows) == 0 {
		return heading
	}

	for index := range rows {
		prefix := "    "
		if index == 0 {
			prefix = "  └ "
		}
		if prefixWidth := lipgloss.Width(prefix); width <= prefixWidth {
			rows[index] = ansi.Truncate(rows[index], width, "…")
		} else {
			rows[index] = prefix + ansi.Truncate(rows[index], width-prefixWidth, "…")
		}
		if !noColor {
			rows[index] = lipgloss.NewStyle().
				Foreground(toolActivityRowColor(
					class,
					index,
					block.tools,
					exploreLimit,
					paletteFor(theme),
				)).
				Render(rows[index])
		}
	}

	return heading + "\n" + strings.Join(rows, "\n")
}

func toolActivityRowColor(
	class toolActivityClass,
	index int,
	activities []toolActivity,
	exploreLimit int,
	palette colorPalette,
) color.Color {
	if class == toolClassPatch && len(activities) > 0 && index < len(activities[0].preview) {
		row := strings.TrimLeft(activities[0].preview[index], " ")
		switch {
		case strings.HasPrefix(row, "A "), strings.HasPrefix(row, "+"):
			return palette.idle
		case strings.HasPrefix(row, "D "), strings.HasPrefix(row, "-"):
			return palette.error
		case strings.HasPrefix(row, "M "):
			return palette.active
		default:
			return palette.muted
		}
	}
	if class != toolClassExplore || index >= len(activities) ||
		(exploreLimit > 0 && len(activities) > exploreLimit && index == exploreLimit) {
		return palette.muted
	}

	switch activities[index].state {
	case toolStateRunning:
		return palette.model
	case toolStateFailed:
		return palette.error
	case toolStateInterrupted:
		return palette.warning
	case toolStateSucceeded:
		return palette.muted
	default:
		return palette.muted
	}
}

func combinedToolState(activities []toolActivity) toolActivityState {
	state := toolStateSucceeded
	for _, activity := range activities {
		switch activity.state {
		case toolStateRunning:
			return toolStateRunning
		case toolStateFailed:
			state = toolStateFailed
		case toolStateInterrupted:
			if state != toolStateFailed {
				state = toolStateInterrupted
			}
		case toolStateSucceeded:
		}
	}

	return state
}

//nolint:gocyclo // The closed class/state matrix is clearer as one visual grammar.
func toolActivityHeading(
	class toolActivityClass,
	state toolActivityState,
	activities []toolActivity,
) (string, string, string) {
	glyph := toolActivityGlyph(state)
	activity := activities[0]

	switch class {
	case toolClassExplore:
		switch state {
		case toolStateRunning:
			return glyph, "Exploring", ""
		case toolStateFailed:
			return glyph, "Explored with errors", ""
		case toolStateInterrupted:
			return glyph, "Exploration interrupted", ""
		case toolStateSucceeded:
			return glyph, "Explored", ""
		}
	case toolClassShell:
		verb := "Ran"
		switch state {
		case toolStateRunning:
			verb = "Running"
		case toolStateFailed:
			if activity.hasHeader && activity.header.Code == "invalid_argument" &&
				activity.header.Execution == nil {
				verb = "Tool input rejected"
			} else {
				verb = "Run failed"
			}
		case toolStateInterrupted:
			verb = "Run interrupted"
		case toolStateSucceeded:
		}
		subject := activity.subject
		if state == toolStateFailed && activity.hasHeader && activity.header.Execution != nil &&
			activity.header.Execution.ExitCode != nil {
			subject += fmt.Sprintf(" · exit %d", *activity.header.Execution.ExitCode)
		}

		return glyph, verb, subject
	case toolClassPatch:
		if state == toolStateRunning {
			return glyph, "Updating workspace", ""
		}
		if state == toolStateSucceeded {
			if verb, subject, ok := singlePatchChange(activity); ok {
				return glyph, verb, subject
			}
		}
		verb := "Updated workspace"
		if activity.hasHeader && activity.header.Counts.Files > 0 {
			label := "files"
			if activity.header.Counts.Files == 1 {
				label = fileCountSingular
			}
			verb = fmt.Sprintf("Updated %d %s", activity.header.Counts.Files, label)
		}
		switch state {
		case toolStateFailed:
			verb = "Workspace update failed"
		case toolStateInterrupted:
			verb = "Workspace update interrupted"
		case toolStateRunning, toolStateSucceeded:
		}

		return glyph, verb, ""
	case toolClassGeneric:
		verb := "Called"
		switch state {
		case toolStateRunning:
			verb = "Calling"
		case toolStateFailed:
			verb = "Call failed"
		case toolStateInterrupted:
			verb = "Call interrupted"
		case toolStateSucceeded:
		}

		return glyph, verb, ""
	case toolClassPlan:
		if state == toolStateFailed || state == toolStateInterrupted {
			return glyph, activity.action + " · " + toolActivityReason(activity), activity.subject
		}

		return glyph, activity.action, activity.subject
	case toolClassSubagent:
		return glyph, activity.action, activity.subject
	}

	return glyph, activity.action, activity.subject
}

func toolActivityGlyph(state toolActivityState) string {
	switch state {
	case toolStateRunning:
		return "✻"
	case toolStateSucceeded:
		return "•"
	case toolStateFailed:
		return "✗"
	case toolStateInterrupted:
		return "!"
	default:
		return "·"
	}
}

func renderToolHeading(
	glyph, verb, subject string,
	state toolActivityState,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	prefix := glyph + " " + verb
	heading := prefix
	if subject != "" {
		heading += " " + subject
	}
	if noColor {
		return ansi.Truncate(heading, max(1, width), "…")
	}

	palette := paletteFor(theme)
	tone := palette.idle
	switch state {
	case toolStateRunning:
		tone = palette.model
	case toolStateFailed:
		tone = palette.error
	case toolStateInterrupted:
		tone = palette.warning
	case toolStateSucceeded:
	}

	heading = lipgloss.NewStyle().Bold(true).Foreground(tone).Render(prefix)
	if subject != "" {
		heading += lipgloss.NewStyle().Foreground(palette.workspace).Render(" " + subject)
	}

	return ansi.Truncate(heading, max(1, width), "…")
}

func toolActivityRows(
	class toolActivityClass,
	activities []toolActivity,
	exploreLimit int,
) []string {
	switch class {
	case toolClassExplore:
		visible := len(activities)
		if exploreLimit > 0 {
			visible = min(visible, exploreLimit)
		}
		rows := make([]string, 0, visible)
		for _, activity := range activities[:visible] {
			row := strings.TrimSpace(activity.action + " " + activity.subject)
			if activity.state == toolStateFailed || activity.state == toolStateInterrupted {
				row += " · " + toolActivityReason(activity)
			}
			rows = append(rows, row)
		}
		if omitted := len(activities) - len(rows); omitted > 0 {
			rows = append(rows, fmt.Sprintf("… %d more actions (ctrl+t for details)", omitted))
		}

		return rows
	case toolClassShell:
		return activities[0].preview
	case toolClassPatch:
		if _, parsed := parsePatchDisplayChanges(activities[0]); !parsed {
			if _, _, single := singlePatchChange(activities[0]); single {
				return nil
			}
		}
		if len(activities[0].preview) == 0 {
			return nil
		}

		return activities[0].preview
	case toolClassGeneric:
		return append([]string{activities[0].invocation}, activities[0].preview...)
	case toolClassSubagent:
		return activities[0].preview
	default:
		return nil
	}
}

func toolActivityReason(activity toolActivity) string {
	if activity.hasHeader {
		if code := humanizeStatusCode(activity.header.Code); code != "" {
			return code
		}
	}
	if activity.state == toolStateInterrupted {
		return "interrupted"
	}

	return toolStateTextFailed
}

func singlePatchChange(activity toolActivity) (verb, subject string, ok bool) {
	if activity.state != toolStateSucceeded || !activity.hasHeader ||
		activity.header.Counts.Files != 1 {
		return "", "", false
	}
	lines := patchResultLines(activity.body, 2)
	if len(lines) != 1 || len(lines[0]) < 3 || lines[0][1] != ' ' {
		return "", "", false
	}

	subject = safeWorkspaceToolPath(strings.TrimSpace(lines[0][2:]))
	if subject == "" || subject == "[invalid path]" {
		return "", "", false
	}
	switch lines[0][0] {
	case 'A':
		return "Added", subject, true
	case 'M':
		return "Edited", subject, true
	case 'D':
		return "Deleted", subject, true
	default:
		return "", "", false
	}
}

func toolActivityIDs(block timelineBlock) []string {
	ids := make([]string, 0, len(block.tools))
	for _, activity := range block.tools {
		if activity.id != "" {
			ids = append(ids, activity.id)
		}
	}

	return ids
}

func redactToolArguments(arguments ai.JSON) string {
	if len(arguments) == 0 {
		return "{}"
	}

	var value any
	if json.Unmarshal(arguments, &value) != nil {
		return "[invalid arguments omitted]"
	}
	if _, ok := value.(map[string]any); !ok {
		return "[non-object arguments omitted]"
	}

	redacted := redactToolValue(value)
	data, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		return "{}"
	}

	return truncateText(string(data), maximumToolDetailBytes)
}

func redactToolValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if isSensitiveToolKey(key) {
				result[key] = "[redacted]"

				continue
			}
			result[key] = redactToolValue(child)
		}

		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = redactToolValue(child)
		}

		return result
	case string:
		if containsSensitiveToolText(typed) {
			return "[redacted]"
		}

		return typed
	default:
		return typed
	}
}

func projectDurableSubagent(activity toolActivity) (coding.SubagentState, bool) {
	if activity.name != subagent.ToolName {
		return coding.SubagentState{}, false
	}

	var arguments durableSubagentArgs
	if json.Unmarshal(activity.arguments, &arguments) != nil ||
		!validSubagentRole(arguments.Role) {
		return coding.SubagentState{}, false
	}

	value := coding.SubagentState{
		ParentRunID: activity.runID,
		Role:        arguments.Role, State: subagent.StateRunning,
		TaskPreview: oneLineSubagentTask(arguments.Task),
	}
	if activity.state == toolStateRunning {
		return value, true
	}

	result, ok := decodeDurableSubagentResult(activity.result)
	if !ok || result.Role != arguments.Role {
		value.State = subagent.StateFailed
		if activity.state == toolStateInterrupted {
			value.State = subagent.StateInterrupted
		}
		value.Code = "invalid_result"

		return value, true
	}

	value.ChildSessionID = result.ChildSessionID
	value.Role = result.Role
	value.State = subagentStateFromOutcome(result.Outcome)
	value.Code = result.Code
	value.Turns = result.Turns
	value.ToolCalls = result.ToolCalls
	value.DurationMillis = result.DurationMillis
	value.Usage = coding.TokenUsage{
		InputTokens:       result.Usage.InputTokens,
		OutputTokens:      result.Usage.OutputTokens,
		ReasoningTokens:   result.Usage.ReasoningTokens,
		CachedInputTokens: result.Usage.CachedInputTokens,
		CacheWriteTokens:  result.Usage.CacheWriteTokens,
	}

	return value, true
}

func decodeDurableSubagentResult(value string) (durableSubagentResult, bool) {
	for offset := strings.IndexByte(value, '{'); offset >= 0; {
		var result durableSubagentResult
		decoder := json.NewDecoder(strings.NewReader(value[offset:]))
		if decoder.Decode(&result) == nil && result.Schema == subagent.ResultSchema &&
			result.ChildSessionID != "" && validSubagentRole(result.Role) {
			return result, true
		}

		next := strings.IndexByte(value[offset+1:], '{')
		if next < 0 {
			break
		}
		offset += next + 1
	}

	return durableSubagentResult{}, false
}

func validSubagentRole(role subagent.Role) bool {
	return role == subagent.RoleExplore || role == subagent.RolePlan ||
		role == subagent.RoleReview
}

func subagentStateFromOutcome(outcome subagent.Outcome) subagent.State {
	switch outcome {
	case subagent.OutcomeSucceeded:
		return subagent.StateSucceeded
	case subagent.OutcomeCanceled:
		return subagent.StateCanceled
	case subagent.OutcomeInterrupted:
		return subagent.StateInterrupted
	case subagent.OutcomeFailed:
		return subagent.StateFailed
	default:
		return subagent.StateFailed
	}
}
