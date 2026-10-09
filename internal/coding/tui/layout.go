//nolint:wsl_v5 // Region caps keep each degradation rung adjacent to its thresholds.
package tui

import (
	"charm.land/lipgloss/v2"
)

// The ready chat frame is composed from vertical bands, top to bottom:
//
//	transcript | prompt | band | composer | status
//
// Every band receives an explicit height before composition, and the composed
// frame is bounded to the terminal height by clampFrame. The transcript is the
// only elastic band: it absorbs whatever the fixed bands leave. A band's budget
// counts everything that band draws, including a bordered composer box.
const (
	// layoutStatusRows is the status band height; the status line never wraps.
	layoutStatusRows = 1
	// layoutBandRows is the reserved band between the activity line and the
	// Composer. The band is one row on every frame — the transcript's elastic
	// height is computed after it — so the Composer never moves when the band's
	// contents change. It is the same row the frame used to leave blank there,
	// which is why reserving it explicitly moves no existing frame.
	layoutBandRows = 1
	// layoutComposerBorderRows is the rounded border contributed by composerBox.
	layoutComposerBorderRows = 2
	// layoutComposerMinRows keeps one editable row even in a short window.
	layoutComposerMinRows = 1
	// layoutUnbounded lets a band take every remaining row. A comfortable window
	// has room for the full Team panel list, so that band is only capped once the
	// window is actually short.
	layoutUnbounded = 1 << 20
)

// Degradation rungs. Below the first threshold the frame is short enough that
// generous caps can push the whole footer past the window, so each rung trades
// secondary information (padding, activity line, picker rows, the composer box
// border) for the interaction that still has to work.
const (
	layoutRoomComfortable = 16
	layoutRoomCompact     = 9
	layoutRoomTight       = 5
	layoutRoomMinimal     = 3
)

// The picker band is capped on every rung, so opening a list never hides the
// conversation the chosen entry is about to act on.
const (
	// layoutPickerHeightPercent is the share of the window a picker band may
	// cover, mirroring fzf's `--height 40%` guidance.
	layoutPickerHeightPercent = 40
	// layoutPickerMaxRows is the ceiling for a palette list: the reference
	// palettes scroll their rows instead of growing (fzf `--height 40%`, VS Code
	// quick picks, comparable TUIs at 8-12 rows).
	layoutPickerMaxRows = 12
)

// layoutCaps are the maximum rows each band may occupy at one window height.
// A zero-height transcript is a valid outcome: the composer and status line keep
// working even when there is no room to show history.
type layoutCaps struct {
	composerRows int  // composer band, including any border
	promptRows   int  // approval/question/plan-review prompt band
	pickerRows   int  // picker panel band
	panelRows    int  // team panel band
	activity     bool // whether the activity line may be shown
	composerMode bool // whether the Composer's bottom border may name the permission mode
	interactive  bool // whether an input surface is drawn at all
}

// layoutCapsFor resolves the degradation ladder for one terminal height.
func layoutCapsFor(height int) layoutCaps {
	caps := layoutCaps{}

	switch {
	case height >= layoutRoomComfortable:
		caps = layoutCaps{
			composerRows: composerMaxLines + layoutComposerBorderRows,
			promptRows:   layoutRoomComfortable,
			pickerRows:   layoutUnbounded,
			panelRows:    layoutUnbounded,
			activity:     true,
			composerMode: true,
			interactive:  true,
		}
	case height >= layoutRoomCompact:
		caps = layoutCaps{
			composerRows: 4 + layoutComposerBorderRows,
			promptRows:   3,
			pickerRows:   3,
			panelRows:    3,
			activity:     true,
			interactive:  true,
		}
	case height >= layoutRoomTight:
		caps = layoutCaps{
			composerRows: 3,
			promptRows:   1,
			pickerRows:   1,
			panelRows:    1,
			activity:     false,
			interactive:  true,
		}
	case height >= layoutRoomMinimal:
		caps = layoutCaps{
			composerRows: 2,
			promptRows:   1,
			pickerRows:   0,
			panelRows:    0,
			activity:     false,
			interactive:  true,
		}
	default:
		caps = layoutCaps{interactive: false}
	}

	// The picker band may take less than its tier allows, never more.
	caps.pickerRows = min(caps.pickerRows, pickerRowsFor(height))

	return caps
}

// pickerRowsFor bounds how much of the window a picker band may cover. A list or
// panel that swallowed the window would hide the conversation the chosen entry is
// about to act on — and the frame's own timeline with it — so the band takes at
// most two fifths of the window, the share fzf ships as `--height 40%`.
func pickerRowsFor(height int) int {
	if height <= 0 {
		return 0
	}

	return max(1, height*layoutPickerHeightPercent/100)
}

// pickerBandRows resolves the band one open picker may draw at the current
// geometry. Every picker is bounded by the window's share above; a palette list
// is additionally capped at layoutPickerMaxRows, because the reference palettes
// scroll their rows rather than growing. The permissions picker is a settings
// form rather than a palette, so it keeps the band's own budget.
func (m *Model) pickerBandRows(caps layoutCaps, available int) int {
	rows := min(caps.pickerRows, available)
	if m.picker.kind != pickerPermissions {
		rows = min(rows, layoutPickerMaxRows)
	}

	return max(1, rows)
}

// drawsComposerBox reports whether the current geometry leaves room for the
// bordered composer. The border is decorative: dropping it is always preferable
// to losing an editable row.
func (m *Model) drawsComposerBox(caps layoutCaps) bool {
	return m.hasComposerBox() &&
		caps.composerRows-layoutComposerBorderRows >= layoutComposerMinRows
}

// layout returns the band budget for the current window.
func (m *Model) layout() layoutCaps {
	return layoutCapsFor(m.height)
}

// composerEditorRows reports the editor rows the composer may use at the current
// geometry, excluding any border.
func (m *Model) composerEditorRows() int {
	caps := m.layout()
	rows := caps.composerRows
	if m.drawsComposerBox(caps) {
		rows -= layoutComposerBorderRows
	}

	return max(layoutComposerMinRows, min(composerMaxLines, rows))
}

// itoa renders a non-negative row/position counter without importing strconv
// into the layout hot path.
func itoa(value int) string {
	if value <= 0 {
		return "0"
	}

	digits := make([]byte, 0, 6)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}

	return string(digits)
}

// clampFrame bounds a composed frame to the terminal height. Band budgets should
// already fit; this is the backstop that makes "the frame is never taller than
// the window" structural instead of arithmetic every surface has to repeat.
func clampFrame(content string, height int) string {
	if height <= 0 {
		return ""
	}
	if lipgloss.Height(content) <= height {
		return content
	}

	return truncateHeight(content, height)
}

// clampFrameTail bounds a frame by keeping its final rows. Surfaces whose most
// important content is at the bottom (the composer and status line) use this so
// an over-tall frame loses ambient rows rather than the controls.
func clampFrameTail(content string, height int) string {
	return truncateTailHeight(content, height)
}
