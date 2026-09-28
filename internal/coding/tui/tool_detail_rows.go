//nolint:wsl_v5 // Row layout, wrapping, and tone styling stay adjacent per row kind.
package tui

import (
	"image/color"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A detail row is plain, sanitized text plus a semantic tone. Wrapping and
// coloring happen at the route boundary, so NO_COLOR and colored output always
// produce the same physical rows.

const (
	detailLabelColumn     = 18
	detailIndentWidth     = 2
	detailMinWrapWidth    = 8
	detailTreeInlineLimit = 8
	detailTreeInlineWidth = 60
	detailListMarker      = "- "
)

type toolDetailTone uint8

const (
	detailToneBody toolDetailTone = iota
	detailToneHeading
	detailToneLabel
	detailToneKey
	detailToneCode
	detailToneAdded
	detailToneRemoved
	detailToneFile
	detailToneMuted
)

// toolDetailRow is one logical line of the detail surface. When key is set it
// renders in a padded muted column, and wrapped continuation lines keep that
// column so the value block stays aligned.
type toolDetailRow struct {
	tone   toolDetailTone
	state  toolActivityState
	indent int
	key    string
	text   string
}

// line renders the row without a frame width, so the plain text and the
// unwrapped inspection never shorten a key.
func (row toolDetailRow) line() string {
	prefix, _ := row.columns(0)

	return prefix + row.text
}

// columns returns the row's key column and the matching blank column used by
// wrapped continuation lines. width is the frame width, or 0 when the row is
// rendered without one. A key wider than the frame is truncated so the value
// still has room, which keeps every rendered line inside the terminal.
func (row toolDetailRow) columns(width int) (prefix, continuation string) {
	indent := strings.Repeat(" ", row.indent*detailIndentWidth)
	if row.key == "" {
		return indent, indent
	}

	key := row.key
	maxKey := width - ansi.StringWidth(indent) - detailMinWrapWidth - 1
	if width > 0 && maxKey < 1 {
		// No room for a labelled column: the value takes the whole row.
		return indent, indent
	}
	if width > 0 && ansi.StringWidth(key) > maxKey {
		key = ansi.Truncate(key, maxKey, "…")
	}

	padding := max(1, detailLabelColumn-ansi.StringWidth(key))

	return indent + key + strings.Repeat(" ", padding), indent + strings.Repeat(" ", ansi.StringWidth(key)+padding)
}

func detailSection(title string) toolDetailRow {
	return toolDetailRow{tone: detailToneLabel, text: title}
}

func detailHeadingRow(glyph, verb, subject string, state toolActivityState) toolDetailRow {
	heading := glyph + " " + verb
	if subject != "" {
		heading += " " + subject
	}

	return toolDetailRow{tone: detailToneHeading, state: state, text: heading}
}

func detailNoteRow(text string) toolDetailRow {
	return toolDetailRow{tone: detailToneMuted, indent: 1, text: text}
}

func detailValueRows(key, value string, tone toolDetailTone, indent int) []toolDetailRow {
	value = sanitizeToolText(value)
	lines := strings.Split(value, "\n")
	rows := make([]toolDetailRow, 0, len(lines))

	for index, line := range lines {
		current := ""
		if index == 0 {
			current = key
		}
		rows = append(rows, toolDetailRow{tone: tone, indent: indent, key: current, text: line})
	}

	return rows
}

func detailCodeRows(key, value string, tone toolDetailTone, indent int) []toolDetailRow {
	return detailValueRows(key, value, tone, indent)
}

// detailTreeRows renders one decoded JSON value as key/value rows. Multi-line
// scalars keep their real newlines (no `\n` escapes) and nested containers
// indent by one level.
func detailTreeRows(key string, value jsonTreeValue, indent int, budget *detailByteBudget) []toolDetailRow {
	if budget.exhausted() {
		return []toolDetailRow{detailTruncationRow(indent)}
	}

	switch value.kind {
	case jsonTreeScalar:
		// A scalar is truncated against, and charged to, the remaining document
		// budget so one oversized value cannot crowd out later sections.
		scalar := truncateText(value.scalar, budget.remainingBytes())
		rows := detailValueRows(key, scalar, detailToneBody, indent)
		budget.spend(len(scalar))
		if budget.exhausted() {
			rows = append(rows, detailTruncationRow(indent))
		}

		return rows
	case jsonTreeList:
		return detailListRows(key, value.list, indent, budget)
	case jsonTreeObject:
		return detailObjectRows(key, value.fields, indent, budget)
	case jsonTreeOmitted:
		if key == "" {
			return []toolDetailRow{{tone: detailToneMuted, indent: indent, text: "…"}}
		}

		return []toolDetailRow{{tone: detailToneMuted, indent: indent, key: key, text: "…"}}
	default:
		return nil
	}
}

func detailObjectRows(key string, fields []jsonTreeField, indent int, budget *detailByteBudget) []toolDetailRow {
	if len(fields) == 0 {
		if key == "" {
			return []toolDetailRow{{tone: detailToneMuted, indent: indent, text: "{}"}}
		}

		return []toolDetailRow{{tone: detailToneMuted, indent: indent, key: key, text: "{}"}}
	}

	rows := make([]toolDetailRow, 0, len(fields)+1)
	if key != "" {
		rows = append(rows, toolDetailRow{tone: detailToneKey, indent: indent, text: key})
		indent++
	}

	for _, field := range fields {
		budget.spend(len(field.key))
		rendered := detailTreeRows(field.key, field.value, indent, budget)
		rows = append(rows, rendered...)
		for _, row := range rendered {
			budget.spend(len(row.text))
		}
		if budget.exhausted() {
			return append(rows, detailTruncationRow(indent))
		}
	}

	return rows
}

func detailListRows(key string, values []jsonTreeValue, indent int, budget *detailByteBudget) []toolDetailRow {
	if len(values) == 0 {
		if key == "" {
			return []toolDetailRow{{tone: detailToneMuted, indent: indent, text: "[]"}}
		}

		return []toolDetailRow{{tone: detailToneMuted, indent: indent, key: key, text: "[]"}}
	}
	if inline, ok := detailInlineList(key, indent, values); ok {
		return inline
	}

	rows := make([]toolDetailRow, 0, len(values)+1)
	if key != "" {
		rows = append(rows, toolDetailRow{tone: detailToneKey, indent: indent, text: key})
		indent++
	}

	for _, value := range values {
		rows = append(rows, detailListItemRows(value, indent, budget)...)
		if budget.exhausted() {
			return append(rows, detailTruncationRow(indent))
		}
	}

	return rows
}

func detailListItemRows(value jsonTreeValue, indent int, budget *detailByteBudget) []toolDetailRow {
	if value.kind == jsonTreeScalar {
		rows := detailValueRows("", value.scalar, detailToneBody, indent)
		if len(rows) == 0 {
			return nil
		}
		rows[0].text = detailListMarker + rows[0].text

		return rows
	}
	if value.kind == jsonTreeObject && len(value.fields) > 0 {
		return detailObjectItemRows(value.fields, indent, budget)
	}

	return detailTreeRows("", value, indent, budget)
}

// detailObjectItemRows renders one array item that is itself an object: the
// marker shares the first field's row and the remaining fields stay in the
// same value column.
func detailObjectItemRows(fields []jsonTreeField, indent int, budget *detailByteBudget) []toolDetailRow {
	rows := detailObjectRows("", fields, indent, budget)
	if len(rows) == 0 {
		return nil
	}
	switch {
	case rows[0].key != "":
		rows[0].key = detailListMarker + rows[0].key
	case rows[0].text != "":
		rows[0].text = detailListMarker + rows[0].text
	}
	for index := 1; index < len(rows); index++ {
		if rows[index].key != "" {
			rows[index].key = "  " + rows[index].key

			continue
		}
		rows[index].text = "  " + rows[index].text
	}

	return rows
}

func detailInlineList(key string, indent int, values []jsonTreeValue) ([]toolDetailRow, bool) {
	if len(values) > detailTreeInlineLimit {
		return nil, false
	}

	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value.kind != jsonTreeScalar || strings.Contains(value.scalar, "\n") {
			return nil, false
		}
		parts = append(parts, oneLineJSONScalar(value.scalar))
	}
	joined := strings.Join(parts, ", ")
	if utf8.RuneCountInString(joined) > detailTreeInlineWidth {
		return nil, false
	}

	return []toolDetailRow{{tone: detailToneBody, indent: indent, key: key, text: joined}}, true
}

func detailTruncationRow(indent int) toolDetailRow {
	return toolDetailRow{tone: detailToneMuted, indent: indent, text: "… truncated …"}
}

// detailByteBudget bounds the text one section of a detail document may
// render, measured over the rendered text rather than an encoded blob. A child
// budget caps one section without consuming its parent, so one oversized
// argument value can never crowd out the Result or Error body; the assembled
// document is bounded separately by clampDetailRows.
type detailByteBudget struct {
	remaining int
}

func newDetailByteBudget(maximum int) *detailByteBudget {
	return &detailByteBudget{remaining: maximum}
}

// child returns an independent budget capped by the parent's remaining room.
func (budget *detailByteBudget) child(limit int) *detailByteBudget {
	if budget == nil {
		return nil
	}

	return &detailByteBudget{remaining: min(limit, budget.remaining)}
}

func (budget *detailByteBudget) spend(bytes int) {
	if budget == nil {
		return
	}

	budget.remaining -= bytes
}

func (budget *detailByteBudget) exhausted() bool {
	return budget == nil || budget.remaining <= 0
}

func (budget *detailByteBudget) remainingBytes() int {
	if budget == nil {
		return 0
	}

	return max(0, budget.remaining)
}

// detailPhysicalRows wraps every logical row to the terminal width and applies
// its tone. Wrapping happens before styling so both modes produce identical
// rows.
func detailPhysicalRows(
	rows []toolDetailRow,
	width int,
	theme colorTheme,
	noColor bool,
) []string {
	width = max(1, width)
	result := make([]string, 0, len(rows))

	for _, row := range rows {
		for _, line := range detailWrapRow(row, width) {
			if noColor {
				result = append(result, line)

				continue
			}

			result = append(result, lipgloss.NewStyle().
				Foreground(detailToneColor(row.tone, row.state, theme)).
				Render(line))
		}
	}

	return result
}

func detailWrapRow(row toolDetailRow, width int) []string {
	prefix, continuation := row.columns(width)
	text := strings.TrimRight(row.text, " ")
	if text == "" {
		return []string{ansi.Truncate(prefix, width, "")}
	}

	available := max(1, width-ansi.StringWidth(prefix))
	if ansi.StringWidth(text) <= available {
		return []string{ansi.Truncate(prefix+text, width, "…")}
	}

	wrapped := ansi.Wrap(text, available, "")
	lines := strings.Split(wrapped, "\n")
	result := make([]string, 0, len(lines))

	for index, line := range lines {
		current := continuation
		if index == 0 {
			current = prefix
		}
		result = append(result, ansi.Truncate(current+line, width, "…"))
	}

	return result
}

func detailToneColor(
	tone toolDetailTone,
	state toolActivityState,
	theme colorTheme,
) color.Color {
	palette := paletteFor(theme)

	switch tone {
	case detailToneHeading:
		switch state {
		case toolStateRunning:
			return palette.model
		case toolStateFailed:
			return palette.error
		case toolStateInterrupted:
			return palette.warning
		case toolStateSucceeded:
			return palette.idle
		}

		return palette.idle
	case detailToneLabel:
		return palette.active
	case detailToneKey, detailToneFile:
		return palette.session
	case detailToneCode:
		return palette.code
	case detailToneAdded:
		return palette.change
	case detailToneRemoved:
		return palette.error
	case detailToneMuted:
		return palette.muted
	default:
		return palette.workspace
	}
}
