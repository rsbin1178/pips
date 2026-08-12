package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/stretchr/testify/assert"
)

func TestWorkspaceChangeBlockShowsBoundedSemanticSummary(t *testing.T) {
	t.Parallel()

	entries := []coding.WorkspaceChange{
		{Path: "added.go", Kind: changes.KindAdded},
		{
			Path: "new-name.go", PreviousPath: "old-name.go",
			Kind: changes.KindRenamed,
		},
		{Path: "deleted.go", Kind: changes.KindDeleted},
		{Path: "conflicted.go", Kind: changes.KindConflict},
	}
	for index := 4; index < 14; index++ {
		entries = append(entries, coding.WorkspaceChange{
			Path: fmt.Sprintf("path-%02d.go", index), Kind: changes.KindModified,
		})
	}

	block := projectWorkspaceChangeBlock(coding.WorkspaceChanged{
		Entries: entries, Files: 16, Additions: 20, Deletions: 4, Truncated: true,
	}, 1)

	rendered := renderWorkspaceChangeBlock(block, 80, themeDark, true)
	assert.Contains(t, rendered, "Workspace changes · 16 files (+20 -4) · partial report")
	assert.Contains(t, rendered, "A  added.go")
	assert.Contains(t, rendered, "R  old-name.go -> new-name.go")
	assert.Contains(t, rendered, "D  deleted.go")
	assert.Contains(t, rendered, "U  conflicted.go")
	assert.Contains(t, rendered, "… 4 more files · /status for repository summary")
	assert.NotContains(t, rendered, "path-12.go")

	narrow := renderWorkspaceChangeBlock(block, 28, themeDark, false)
	assert.Contains(t, narrow, "\x1b[")

	for line := range strings.SplitSeq(narrow, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 28)
	}
}

func TestWorkspaceChangeBlockSupportsLegacyAndEmptyReports(t *testing.T) {
	t.Parallel()

	legacy := projectWorkspaceChangeBlock(coding.WorkspaceChanged{
		Entries: []coding.WorkspaceChange{{
			Path: "legacy.go", Kind: changes.KindModified,
		}},
	}, 1)
	rendered := renderWorkspaceChangeBlock(legacy, 80, themeDark, true)
	assert.Contains(t, rendered, "Workspace changes · 1 file")
	assert.Contains(t, rendered, "M  legacy.go")

	empty := projectWorkspaceChangeBlock(coding.WorkspaceChanged{}, 1)
	rendered = renderWorkspaceChangeBlock(empty, 80, themeDark, true)
	assert.Equal(t, "Workspace changes · 0 files", rendered)
}
