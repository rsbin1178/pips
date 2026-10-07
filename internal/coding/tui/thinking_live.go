//nolint:wsl_v5 // The frozen rows and their alignment padding are written together.
package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A live Thinking block grows with every streamed delta, and re-wrapping,
// re-indenting and re-styling the whole thought on each frame is O(body) per
// frame and O(body^2) over one message. The renderer instead freezes the rows up
// to the last line break and renders only what follows it.
//
// Two properties make the split exact, and both are pinned by tests:
//
//   - ansi.Wrap(body) == ansi.Wrap(body[:k]) + ansi.Wrap(body[k:]) when body[:k]
//     ends on a line break, because the wrapper resets its line state there;
//   - lipgloss pads every line of a rendered block to the widest line, so
//     Style.Render(block) == join(Style.Render(line) + padding(line)).
//
// A thought without a line break has nowhere to freeze, so it still renders
// whole; the section is plain prose, so that only happens while the model has
// not finished its first line.
type liveThinking struct {
	// source is the sanitized body prefix the frozen rows cover. It always ends
	// on a line break, and it shares the caller's backing array.
	source string
	// rows are the frozen rows with the section's glyph or indent applied,
	// styled their styled form, and widths each row's display width.
	rows   []string
	styled []string
	widths []int
	// padded holds the frozen rows padded to the block width and inset, so a frame
	// hands them to the store without re-padding and re-insetting every frozen
	// row. paddedWidth is the width they were padded to (-1 for the NO_COLOR
	// layout, which pads nothing) and paddedCount how many frozen rows they
	// cover, so a newly frozen row is padded once and only a width change re-pads
	// the prefix.
	padded      []string
	paddedWidth int
	paddedCount int
	// geometry: rows rendered at another wrap width, inset, theme or colour mode
	// cannot be extended.
	limit   int
	inset   int
	theme   themeFingerprint
	noColor bool
}

// thinkingRows wraps one body region and applies the section's leading glyph or
// indent. dropTrailing removes the empty row a trailing line break leaves, which
// the next frozen region continues.
func thinkingRows(region, lead, indent string, limit int, dropTrailing bool) []string {
	rows := strings.Split(ansi.Wrap(region, limit, ""), "\n")
	if dropTrailing && len(rows) > 0 && rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
	}

	for index := range rows {
		if strings.TrimSpace(rows[index]) == "" {
			// A blank separator row carries neither the glyph nor the indent.
			rows[index] = ""

			continue
		}

		if index == 0 {
			rows[index] = lead + rows[index]

			continue
		}

		rows[index] = indent + rows[index]
	}

	return rows
}

// renderLiveThinking renders one frame of the live Thinking block, extending the
// frozen rows rather than wrapping and styling the whole thought again. body is
// already sanitized.
func (r *markdownRenderer) renderLiveThinking(
	body, glyph, indent string,
	limit int,
	theme colorTheme,
	noColor bool,
) string {
	tail := r.freezeLiveThinking(body, glyph, indent, limit, 0, theme, noColor)

	return r.thinking.compose(tail, theme, noColor)
}

// renderLiveThinkingRows is [markdownRenderer.renderLiveThinking] for the managed
// transcript store: it returns the frozen rows and the tail as two segments, each
// padded and inset, so a frame never joins the whole thought or re-pads the
// frozen prefix.
func (r *markdownRenderer) renderLiveThinkingRows(
	body, glyph, indent string,
	limit, inset int,
	theme colorTheme,
	noColor bool,
) rowSegments {
	tail := r.freezeLiveThinking(body, glyph, indent, limit, inset, theme, noColor)

	return r.thinking.composeRows(tail, theme, noColor, inset)
}

// freezeLiveThinking advances the slot's frozen rows to the body's last line
// break and returns the tail rows, with the glyph or indent applied. inset is
// part of the geometry, so a caller that wants inset rows keeps its own prefix.
func (r *markdownRenderer) freezeLiveThinking(
	body, glyph, indent string,
	limit, inset int,
	theme colorTheme,
	noColor bool,
) []string {
	fingerprint := themeFingerprint(theme.fingerprint)

	frozen := &r.thinking
	if frozen.limit != limit || frozen.inset != inset ||
		frozen.theme != fingerprint || frozen.noColor != noColor {
		*frozen = liveThinking{limit: limit, inset: inset, theme: fingerprint, noColor: noColor}
	}

	covered := len(frozen.source)
	if covered > len(body) || !strings.HasPrefix(body, frozen.source) {
		// The body was replaced or rewound: the frozen rows no longer describe it.
		*frozen = liveThinking{limit: limit, inset: inset, theme: fingerprint, noColor: noColor}
		covered = 0
	}

	// Only whole lines are frozen: a line break resets the wrapper's state, so
	// everything before the last one wraps the same in or out of the block.
	if checkpoint := strings.LastIndex(body, "\n") + 1; checkpoint > covered {
		lead := indent
		if covered == 0 {
			lead = glyph
		}

		region := body[covered:checkpoint]
		r.thinkingWrapped += len(region)
		frozen.freeze(thinkingRows(region, lead, indent, limit, true), theme, noColor)
		frozen.source = body[:checkpoint]
		covered = checkpoint
	}

	// A thought with no line break yet has nowhere to freeze, so the whole body
	// is the tail and it still leads with the glyph.
	lead := indent
	if covered == 0 {
		lead = glyph
	}

	tail := body[covered:]
	r.thinkingWrapped += len(tail)

	return thinkingRows(tail, lead, indent, limit, false)
}

// freeze appends one more region's rows to the frozen prefix.
func (f *liveThinking) freeze(rows []string, theme colorTheme, noColor bool) {
	if len(rows) == 0 {
		return
	}

	if noColor {
		f.rows = append(f.rows, rows...)

		return
	}

	style := lipgloss.NewStyle().Foreground(paletteFor(theme).muted)
	for _, row := range rows {
		f.rows = append(f.rows, row)
		f.styled = append(f.styled, style.Render(row))
		f.widths = append(f.widths, ansi.StringWidth(row))
	}
}

// compose writes the frozen rows and the tail as one aligned block. The tail is
// never cached: it is the part still growing.
func (f *liveThinking) compose(tail []string, theme colorTheme, noColor bool) string {
	if noColor {
		rows := make([]string, 0, len(f.rows)+len(tail))
		rows = append(rows, f.rows...)
		rows = append(rows, tail...)

		return strings.Join(rows, "\n")
	}

	style := lipgloss.NewStyle().Foreground(paletteFor(theme).muted)

	styledTail := make([]string, len(tail))
	widthsTail := make([]int, len(tail))
	width := 0
	for _, rowWidth := range f.widths {
		width = max(width, rowWidth)
	}

	for index, row := range tail {
		styledTail[index] = style.Render(row)
		widthsTail[index] = ansi.StringWidth(row)
		width = max(width, widthsTail[index])
	}

	// A block is aligned against its widest row, so one wide row in the tail
	// widens the padding of the frozen rows too. The width is recomputed rather
	// than remembered, because a tail row can be wider than every frozen row.
	// The block is built into one exactly sized buffer, so a frame allocates the
	// joined rows once.
	frozenSize := thinkingBlockSize(f.styled, f.widths, width)
	tailSize := thinkingBlockSize(styledTail, widthsTail, width)

	var builder strings.Builder
	builder.Grow(frozenSize + tailSize + 1)
	writeThinkingRows(&builder, f.styled, f.widths, width, false)
	writeThinkingRows(&builder, styledTail, widthsTail, width, len(f.styled) > 0)

	return builder.String()
}

// composeRows is [liveThinking.compose] for the managed transcript store: it
// returns the frozen rows and the tail as two segments, each padded to the
// block's width and moved in by inset, so a frame hands the store the rows it
// keeps instead of joining the whole thought and re-padding the frozen prefix.
func (f *liveThinking) composeRows(tail []string, theme colorTheme, noColor bool, inset int) rowSegments {
	prefix := strings.Repeat(" ", inset)

	if noColor {
		switch {
		case f.paddedWidth != -1:
			f.padded = insetRowSlice(f.rows, inset)
			f.paddedWidth, f.paddedCount = -1, len(f.rows)
		case f.paddedCount < len(f.rows):
			for index := f.paddedCount; index < len(f.rows); index++ {
				f.padded = append(f.padded, insetThinkingRow(f.rows[index], prefix))
			}

			f.paddedCount = len(f.rows)
		}

		return rowSegments{frozen: f.padded, tail: insetRowSlice(tail, inset)}
	}

	style := lipgloss.NewStyle().Foreground(paletteFor(theme).muted)

	styledTail := make([]string, len(tail))
	widthsTail := make([]int, len(tail))
	width := 0
	for _, rowWidth := range f.widths {
		width = max(width, rowWidth)
	}

	for index, row := range tail {
		styledTail[index] = style.Render(row)
		widthsTail[index] = ansi.StringWidth(row)
		width = max(width, widthsTail[index])
	}

	// The block is aligned against its widest row, so one wide row in the tail
	// widens the padding of the frozen rows too. Padding is cached by width and
	// extended by the newly frozen rows, so a frame only pads the tail.
	switch {
	case f.paddedWidth != width:
		f.padded = make([]string, 0, len(f.styled))
		for index, row := range f.styled {
			f.padded = append(f.padded, insetThinkingRow(thinkingPaddedRow(row, f.widths[index], width), prefix))
		}

		f.paddedWidth, f.paddedCount = width, len(f.styled)
	case f.paddedCount < len(f.styled):
		for index := f.paddedCount; index < len(f.styled); index++ {
			f.padded = append(f.padded, insetThinkingRow(thinkingPaddedRow(f.styled[index], f.widths[index], width), prefix))
		}

		f.paddedCount = len(f.styled)
	}

	paddedTail := make([]string, len(styledTail))
	for index, row := range styledTail {
		paddedTail[index] = insetThinkingRow(thinkingPaddedRow(row, widthsTail[index], width), prefix)
	}

	return rowSegments{frozen: f.padded, tail: paddedTail}
}

// thinkingPaddedRow pads one styled row to width, the way [writeThinkingRows]
// pads the rows it writes.
func thinkingPaddedRow(styled string, rowWidth, width int) string {
	pad := width - rowWidth
	if pad <= 0 {
		return styled
	}

	if pad <= len(thinkingPadding) {
		return styled + thinkingPadding[:pad]
	}

	return styled + strings.Repeat(" ", pad)
}

// insetThinkingRow moves one padded row in from the frame edge. A blank row stays
// empty, which is what [insetRows] does for an empty line.
func insetThinkingRow(row, prefix string) string {
	if prefix == "" || row == "" {
		return row
	}

	return prefix + row
}

// thinkingBlockSize reports the bytes writeThinkingRows writes for these rows,
// so compose can size its buffer exactly.
func thinkingBlockSize(styled []string, widths []int, width int) int {
	size := 0
	for index, row := range styled {
		if index > 0 {
			size++
		}

		size += len(row)
		if pad := width - widths[index]; pad > 0 {
			size += pad
		}
	}

	return size
}

// thinkingPadding serves alignment spaces without allocating one string per row.
const thinkingPadding = "                                                                "

// writeThinkingRows writes styled rows with the padding that brings each to
// width, separating them with line breaks. separator prepends a break when these
// rows continue a block that already wrote some.
func writeThinkingRows(builder *strings.Builder, styled []string, widths []int, width int, separator bool) {
	for index, row := range styled {
		if index > 0 || separator {
			builder.WriteByte('\n')
		}

		builder.WriteString(row)

		for pad := width - widths[index]; pad > 0; pad -= len(thinkingPadding) {
			builder.WriteString(thinkingPadding[:min(pad, len(thinkingPadding))])
		}
	}
}
