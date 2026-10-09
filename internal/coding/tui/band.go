//nolint:wsl_v5 // The band's two occupants and their alignment stay adjacent.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The reserved band is the row between the activity line and the Composer. The
// frame budget counts it on every frame (layoutBandRows), so the Composer never
// moves when the band's contents change: the band is one fixed row that is blank
// until there is something to say.
//
// Two things live there today.
//
//   - A scroll hint, drawn only while the newest transcript rows are off screen.
//     It is the affordance that replaces the status line's scroll hint, and a left
//     click anywhere on the band row restores the newest rows, the way End does.
//   - The confirmation of a drag copy ("copied 3 lines"). The drag path writes
//     the clipboard alone, so it has no file to name; an explicit /copy, /export
//     or path copy keeps its status-line notice, which does name the file. The
//     two surfaces therefore never repeat each other.
const (
	// bandScrollIcon marks the direction the newest rows are in. It is the filled
	// triangle rather than the hollow one, which this UI uses as its attention marker;
	// its up counterpart is the turn indicator at the top of the transcript. The glyph
	// carries no label: the band is a signpost, and the status line already names the
	// keys a reader needs.
	bandScrollIcon = "▼"
)

// copiedNoticeExpiredMsg clears the band's copy confirmation only if it is still
// the current one, so a stale timer cannot erase a newer confirmation.
type copiedNoticeExpiredMsg struct {
	generation uint64
}

// bandScrollHint is the marker the band shows while the newest output is off screen,
// or "" while the reader is already looking at it.
//
// The test is the scroll region's own "at the bottom" rather than its `follow`
// flag: a resize that grows the window clamps the offset onto the new maximum
// without restoring `follow`, and a hint driven by the flag would then point at
// rows that are already on screen.
func (m *Model) bandScrollHint() string {
	if m.transcriptScroll.atBottom() {
		return ""
	}

	return bandScrollIcon
}

// copiedNoticeText is the band's copy confirmation, or "" when none is current.
func (m *Model) copiedNoticeText() string {
	return strings.TrimSpace(sanitizeInspectionText(m.copiedNotice))
}

// reservedBand renders the band's single row. The scroll hint is centred on the
// whole row, because it is a standing invitation to move rather than a field of
// the status line; the copy confirmation keeps the status line's inset and its
// right edge, which is where a transient outcome reads as an outcome.
//
// A narrow terminal truncates the hint's label rather than the outcome, and
// NO_COLOR drops the styling and keeps the text.
//
// The row is blank when there is nothing to show, which is what keeps a frame
// with no live feedback byte-identical to the blank separator row it replaced.
func (m *Model) reservedBand() string {
	hint := m.bandScrollHint()
	notice := m.copiedNoticeText()
	if hint == "" && notice == "" {
		return ""
	}
	if !m.options.NoColor {
		muted := lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted)
		hint = styleWhenNotEmpty(muted, hint)
		notice = styleWhenNotEmpty(muted, notice)
	}

	width := max(1, m.width)
	inset := timelineInset(width)

	switch {
	case hint == "":
		// A copy confirmation keeps the status line's inset and right edge, where a
		// transient outcome reads as an outcome.
		return strings.Repeat(" ", inset) + alignStatusLine("", notice, width-2*inset)
	case notice == "":
		// The scroll hint is centred on the whole row: it is a standing invitation to
		// move rather than a field of the status line.
		return centreLine(hint, width)
	}

	// Both. The row is [hint][gap][confirmation][inset]; the confirmation wins it
	// when the hint could not keep its glyphs.
	noticeField := ansi.StringWidth(notice)
	hintField := width - 1 - noticeField - inset
	if hintField < ansi.StringWidth(bandScrollIcon) {
		return strings.Repeat(" ", inset) + alignStatusLine("", notice, width-2*inset)
	}

	return centreLine(hint, hintField) + " " + notice + strings.Repeat(" ", inset)
}

// centreLine centres value in a field of width columns. The row it returns always
// measures exactly width, so the band stays aligned with the frame edge whatever
// it holds, and an odd remainder leaves the extra column on the right.
func centreLine(value string, width int) string {
	if width <= 0 {
		return ""
	}

	value = ansi.Truncate(value, width, "…")
	padding := width - ansi.StringWidth(value)
	left := padding / 2

	return strings.Repeat(" ", left) + value + strings.Repeat(" ", padding-left)
}

func styleWhenNotEmpty(style lipgloss.Style, value string) string {
	if value == "" {
		return ""
	}

	return style.Render(value)
}

// copiedLinesLabel words how much text a copy took, so the reader can tell a
// one-line pick from a screenful without pasting.
func copiedLinesLabel(lines int) string {
	if lines == 1 {
		return "copied 1 line"
	}

	return fmt.Sprintf("copied %d lines", lines)
}

// showCopiedNotice puts a copy confirmation in the band for as long as a status
// notice lives, so both surfaces expire on the same clock.
func (m *Model) showCopiedNotice(lines int) tea.Cmd {
	m.copiedNoticeSeq++
	m.copiedNotice = copiedLinesLabel(lines)
	m.setLayout()

	generation := m.copiedNoticeSeq

	return tea.Tick(statusNoticeTime, func(time.Time) tea.Msg {
		return copiedNoticeExpiredMsg{generation: generation}
	})
}
