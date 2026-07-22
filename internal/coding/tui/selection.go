//nolint:wsl_v5 // Window expansion keeps each boundary update next to its guard.
package tui

import "strings"

// selectionWindowByHeight returns a contiguous item range that contains the
// cursor and fits the available rendered line budget whenever possible.
func selectionWindowByHeight(heights []int, cursor, maximum int) (int, int) {
	if len(heights) == 0 {
		return 0, 0
	}

	cursor = min(max(0, cursor), len(heights)-1)
	maximum = max(1, maximum)
	start := cursor
	end := cursor + 1
	used := max(1, heights[cursor])
	if used >= maximum {
		return start, end
	}

	for start > 0 || end < len(heights) {
		advanced := false
		if start > 0 {
			height := max(1, heights[start-1])
			if used+height <= maximum {
				start--
				used += height
				advanced = true
			}
		}
		if end < len(heights) {
			height := max(1, heights[end])
			if used+height <= maximum {
				end++
				used += height
				advanced = true
			}
		}
		if !advanced {
			break
		}
	}

	return start, end
}

func truncateHeight(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}

	lines := strings.Split(value, "\n")
	if len(lines) <= maximum {
		return value
	}

	return strings.Join(lines[:maximum], "\n")
}

// truncateTailHeight keeps the newest rendered lines within the managed frame.
// Stable content is committed to native scrollback; only mutable draft content
// reaches this helper, so retaining its tail keeps the live cursor visible.
func truncateTailHeight(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}

	lines := strings.Split(value, "\n")
	if len(lines) <= maximum {
		return value
	}

	return strings.Join(lines[len(lines)-maximum:], "\n")
}
