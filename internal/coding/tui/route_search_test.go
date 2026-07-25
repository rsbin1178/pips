package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
)

func TestRouteSearchBoxStaysSingleLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		noColor bool
	}{
		{name: "color"},
		{name: "no_color", noColor: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := readyModel(t, test.noColor)
			model.route = newSessionPickerState("", themeDark, test.noColor)
			model.route.search.SetWidth(96)

			box, _, _ := model.routeSearchBox(72)
			plain := ansi.Strip(box)

			assert.Equal(t, 3, lipgloss.Height(box))
			assert.Equal(t, 1, strings.Count(plain, "…"))

			for line := range strings.SplitSeq(plain, "\n") {
				assert.LessOrEqual(t, ansi.StringWidth(line), 72)
			}
		})
	}
}
