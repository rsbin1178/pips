//nolint:wsl_v5 // Small surface helpers stay grouped to avoid coupling them to individual routes.
package tui

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func fitScrollableContent(content string, width, height, offset int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > height {
		visible := max(1, height-1)
		start := min(max(0, offset), len(lines)-visible)
		end := min(len(lines), start+visible)
		window := slices.Clone(lines[start:end])
		window = append(window, fmt.Sprintf("… lines %d-%d/%d …", start+1, end, len(lines)))
		lines = window
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, width), "…")
	}

	return strings.Join(lines, "\n")
}

func wrapIndex(value, length int) int {
	if length <= 0 {
		return 0
	}

	return (value%length + length) % length
}

func trimLastRune(value string) string {
	if value == "" {
		return ""
	}

	_, size := utf8.DecodeLastRuneInString(value)

	return value[:len(value)-size]
}
