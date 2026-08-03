package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	codingtools "github.com/rsbin/pips/internal/coding/tools"
	patchdoc "github.com/rsbin/pips/internal/coding/tools/patch"
)

const (
	compactPatchPreviewRows      = 20
	compactPatchPreviewLineBytes = 500
)

type patchDisplayChange struct {
	kind  byte
	path  string
	lines []string
}

func compactPatchDiffPreview(activity toolActivity) ([]string, bool) {
	changes, ok := parsePatchDisplayChanges(activity)
	if !ok {
		return nil, false
	}

	rows := renderPatchDisplayChanges(changes, len(changes) != 1)
	for index := range rows {
		rows[index] = truncateText(rows[index], compactPatchPreviewLineBytes)
	}

	if len(rows) <= compactPatchPreviewRows {
		return rows, true
	}

	visible := compactPatchPreviewRows - 1
	omitted := len(rows) - visible
	rows = append(slices.Clone(rows[:visible]), fmt.Sprintf(
		"… +%d diff rows (ctrl+t for details)", omitted,
	))

	return rows, true
}

func patchDetailChanges(activity toolActivity) string {
	changes, ok := parsePatchDisplayChanges(activity)
	if !ok {
		return ""
	}

	rows := renderPatchDisplayChanges(changes, true)

	return truncateText(strings.Join(rows, "\n"), maximumToolDetailBytes)
}

func parsePatchDisplayChanges(activity toolActivity) ([]patchDisplayChange, bool) {
	if activity.state != toolStateSucceeded || !activity.hasHeader || !activity.header.OK {
		return nil, false
	}

	var arguments struct {
		Patch string `json:"patch"`
	}
	if json.Unmarshal(activity.arguments, &arguments) != nil || arguments.Patch == "" {
		return nil, false
	}

	limits := codingtools.DefaultLimits()

	document, err := patchdoc.Parse(arguments.Patch, patchdoc.Limits{
		Bytes: limits.PatchBytes, Files: limits.PatchFiles,
	})
	if err != nil || len(document.Changes) != activity.header.Counts.Files {
		return nil, false
	}

	result := make([]patchDisplayChange, 0, len(document.Changes))
	for _, change := range document.Changes {
		result = append(result, newPatchDisplayChange(change))
	}

	return result, true
}

func renderPatchDisplayChanges(changes []patchDisplayChange, includeSinglePath bool) []string {
	rows := make([]string, 0)
	includePaths := includeSinglePath || len(changes) != 1

	for _, change := range changes {
		if includePaths {
			rows = append(rows, fmt.Sprintf("%c %s", change.kind, change.path))
		}

		for _, line := range change.lines {
			rows = append(rows, "  "+line)
		}
	}

	return rows
}

func addedPatchLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}

	value := strings.TrimSuffix(string(content), "\n")
	lines := strings.Split(value, "\n")

	result := make([]string, 0, len(lines))
	for _, line := range lines {
		result = append(result, "+"+safePatchCodeText(line))
	}

	return result
}

func newPatchDisplayChange(change patchdoc.Change) patchDisplayChange {
	display := patchDisplayChange{kind: patchDisplayKind(change.Kind), path: change.Path}

	switch change.Kind {
	case patchdoc.Add:
		display.lines = addedPatchLines(change.Content)
	case patchdoc.Update:
		display.lines = updatedPatchLines(change.Hunks)
	case patchdoc.Delete:
	}

	return display
}

func updatedPatchLines(hunks []patchdoc.Hunk) []string {
	var result []string

	for _, hunk := range hunks {
		for _, line := range hunk.Lines {
			if line.Operation != '+' && line.Operation != '-' {
				continue
			}

			result = append(
				result,
				string(line.Operation)+safePatchCodeText(line.Text),
			)
		}
	}

	return result
}

func patchDisplayKind(kind patchdoc.Kind) byte {
	switch kind {
	case patchdoc.Add:
		return 'A'
	case patchdoc.Update:
		return 'M'
	case patchdoc.Delete:
		return 'D'
	default:
		return '?'
	}
}

func safePatchCodeText(value string) string {
	value = ansi.Strip(value)

	var result strings.Builder
	result.Grow(len(value))

	for _, character := range value {
		switch {
		case character == '\t':
			result.WriteString("    ")
		case !unicode.IsControl(character):
			result.WriteRune(character)
		}
	}

	return result.String()
}
