package tui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/changes"
)

const (
	compactWorkspaceChangeRows = 12
	fileCountSingular          = "file"
)

func projectWorkspaceChangeBlock(value coding.WorkspaceChanged, position int) timelineBlock {
	value.Entries = slices.Clone(value.Entries)

	return timelineBlock{
		kind: blockChange, position: position, workspaceChanges: &value,
	}
}

func renderWorkspaceChangeBlock(
	block timelineBlock,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	if block.workspaceChanges == nil {
		return ""
	}

	width = max(1, width)
	value := *block.workspaceChanges

	title := ansi.Truncate(
		"Workspace changes · "+workspaceChangeSummary(value),
		width,
		"…",
	)
	if !noColor {
		title = timelineTitleStyle(blockChange, theme).Render(title)
	}

	lines := []string{title}
	entries := value.Entries

	visible := min(len(entries), compactWorkspaceChangeRows)
	for _, entry := range entries[:visible] {
		line := ansi.Truncate(
			"  "+workspaceChangeGlyph(entry.Kind)+"  "+workspaceChangePath(entry),
			width,
			"…",
		)
		if !noColor {
			line = lipgloss.NewStyle().
				Foreground(workspaceChangeColor(entry.Kind, paletteFor(theme))).
				Render(line)
		}

		lines = append(lines, line)
	}

	files := workspaceChangeFileCount(value)
	omitted := max(0, files-visible)

	hint := "  /status for repository summary"
	if omitted > 0 {
		hint = fmt.Sprintf("  … %d more files · /status for repository summary", omitted)
	}

	if files > 0 {
		hint = ansi.Truncate(hint, width, "…")
		if !noColor {
			hint = lipgloss.NewStyle().Foreground(paletteFor(theme).muted).Render(hint)
		}

		lines = append(lines, hint)
	}

	return strings.Join(lines, "\n")
}

func workspaceChangeSummary(value coding.WorkspaceChanged) string {
	files := workspaceChangeFileCount(value)

	label := "files"
	if files == 1 {
		label = fileCountSingular
	}

	summary := fmt.Sprintf("%d %s", files, label)
	if value.Additions > 0 || value.Deletions > 0 {
		summary += fmt.Sprintf(" (+%d -%d)", value.Additions, value.Deletions)
	}

	if value.Truncated {
		summary += " · partial report"
	}

	return summary
}

func workspaceChangeFileCount(value coding.WorkspaceChanged) int {
	if value.Files > 0 {
		return value.Files
	}

	return len(value.Entries)
}

func workspaceChangeGlyph(kind changes.Kind) string {
	switch kind {
	case changes.KindAdded:
		return "A"
	case changes.KindModified:
		return "M"
	case changes.KindDeleted:
		return "D"
	case changes.KindRenamed:
		return "R"
	case changes.KindUntracked:
		return "?"
	case changes.KindConflict:
		return "U"
	default:
		return "·"
	}
}

func workspaceChangePath(entry coding.WorkspaceChange) string {
	path := safeStatusPath(entry.Path)
	if entry.PreviousPath != "" {
		path = safeStatusPath(entry.PreviousPath) + " -> " + path
	}

	return path
}

func workspaceChangeColor(kind changes.Kind, palette colorPalette) color.Color {
	switch kind {
	case changes.KindAdded, changes.KindUntracked:
		return palette.idle
	case changes.KindDeleted:
		return palette.error
	case changes.KindConflict:
		return palette.warning
	case changes.KindModified, changes.KindRenamed:
		return palette.muted
	default:
		return palette.muted
	}
}
