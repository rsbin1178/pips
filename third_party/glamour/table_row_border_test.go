package glamour

import (
	"strings"
	"testing"

	"charm.land/glamour/v2/styles"
)

const tableRowBorderMarkdown = `
| Header A | Header B |
| -------- | -------- |
| Cell 1   | Cell 2   |
| Cell 3   | Cell 4   |
`

// tableRuleLines counts the horizontal rules a rendered table draws: lines whose
// only runes are border characters and which contain at least one dash.
func tableRuleLines(rendered string) int {
	rules := 0
	for line := range strings.SplitSeq(rendered, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.ContainsRune(trimmed, '-') && strings.Trim(trimmed, "-+|─┼ ") == "" {
			rules++
		}
	}

	return rules
}

func renderRowBorderTable(t *testing.T, rowBorder *bool) string {
	t.Helper()

	style := styles.ASCIIStyleConfig
	style.Table.RowBorder = rowBorder
	renderer, err := NewTermRenderer(WithStyles(style), WithWordWrap(80))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderer.Render(strings.TrimSpace(tableRowBorderMarkdown))
	if err != nil {
		t.Fatal(err)
	}

	return rendered
}

// TestTableRowBorderIsOptIn pins the historical look: a style that leaves the
// toggle unset renders the header rule only, so existing styles are unaffected.
func TestTableRowBorderIsOptIn(t *testing.T) {
	if got := tableRuleLines(renderRowBorderTable(t, nil)); got != 1 {
		t.Errorf("unset RowBorder: want the header rule only (1 rule), got %d", got)
	}
}

// TestTableRowBorderFalseKeepsTheHistoricalLook pins the explicit opt-out.
func TestTableRowBorderFalseKeepsTheHistoricalLook(t *testing.T) {
	disabled := false
	if got := tableRuleLines(renderRowBorderTable(t, &disabled)); got != 1 {
		t.Errorf("RowBorder=false: want the header rule only (1 rule), got %d", got)
	}
}

// TestTableRowBorderDrawsDividers pins the toggle: it adds a rule between every
// pair of body rows on top of the header rule.
func TestTableRowBorderDrawsDividers(t *testing.T) {
	enabled := true
	rendered := renderRowBorderTable(t, &enabled)
	// The header rule plus the divider between the two body rows.
	if got := tableRuleLines(rendered); got != 2 {
		t.Errorf("RowBorder=true: want 2 rules, got %d:\n%s", got, rendered)
	}
	// With the outer frame off, lipgloss must not draw separator end-caps.
	if strings.ContainsAny(rendered, "├┤") {
		t.Errorf("RowBorder=true drew separator end-caps:\n%s", rendered)
	}
}
