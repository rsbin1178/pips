//nolint:wsl_v5 // Model-history fixtures keep their setup and assertions adjacent.
package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statusModelStore writes a store mixing usable, missing and corrupt projections
// with per-day data, and returns the directory and the listed Sessions. s-1 and
// s-2 ran today, s-3 ran only 40 days ago, and s-6 ran both 40 days ago and today
// so its in-range total differs from its all-time total.
func statusModelStore(t *testing.T) (string, []session.Metadata) {
	t.Helper()

	directory := t.TempDir()
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	old := today.AddDate(0, 0, -40)

	require.True(t, writeUsageProjection(
		directory, "s-1", "i-1", 60_000,
		map[string]coding.TokenUsage{"demo/model": {InputTokens: 100, OutputTokens: 20}},
		today,
	).wrote)
	require.True(t, writeUsageProjection(
		directory, "s-2", "i-1", 30_000,
		map[string]coding.TokenUsage{"small/model": {InputTokens: 10}},
		today,
	).wrote)
	require.True(t, writeUsageProjection(
		directory, "s-3", "i-1", 10_000,
		map[string]coding.TokenUsage{"old/model": {InputTokens: 300}},
		old,
	).wrote)
	require.True(t, writeUsageProjection(
		directory, "s-6", "i-1", 10_000,
		map[string]coding.TokenUsage{"mixed/model": {InputTokens: 200}},
		old,
	).wrote)
	require.True(t, writeUsageProjection(
		directory, "s-6", "i-2", 5_000,
		map[string]coding.TokenUsage{"mixed/model": {InputTokens: 50}},
		today,
	).wrote)

	corrupt := filepath.Join(directory, "s-5")
	require.NoError(t, os.MkdirAll(corrupt, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(corrupt, usageProjectionFile), []byte("{not json"), 0o600,
	))

	return directory, []session.Metadata{
		{ID: "s-1"}, {ID: "s-2"}, {ID: "s-3"}, {ID: "s-4"}, {ID: "s-5"}, {ID: "s-6"},
	}
}

// statusModelsPageModel opens the Models page over a store and drives its read.
func statusModelsPageModel(t *testing.T, directory string, metas []session.Metadata) *Model {
	t.Helper()

	controller := newOverlayController(readyState())
	now := time.Now()
	for _, meta := range metas {
		meta.CreatedAt = now
		controller.sessions = append(controller.sessions, meta)
	}
	model := readyModelWithController(t, controller, true)
	model.options.SessionsDirectory = directory
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	model.executeCommand(commandDescriptor{name: commandStatus})
	driveModelCommands(t, model, model.selectStatusTab(statusTabModels))

	return model
}

// TestStatusPanelModelsRendersPerModelRowsAndStrips pins the Models page: one row
// per model with its in-range and all-time totals, a daily strip for the models
// with in-range tokens, the shared range selector and the coverage line.
func TestStatusPanelModelsRendersPerModelRowsAndStrips(t *testing.T) {
	t.Parallel()

	directory, metas := statusModelStore(t)
	model := statusModelsPageModel(t, directory, metas)

	page := statusPageText(model, statusTabModels)
	assert.Contains(t, page, "Models")
	assert.Contains(t, page, "All time", "the range is named")
	assert.Contains(t, page, "old/model: 300 in range · 300 all time")
	assert.Contains(t, page, "mixed/model: 250 in range · 250 all time")
	assert.Contains(t, page, "demo/model: 120 in range · 120 all time")
	assert.Contains(t, page, "small/model: 10 in range · 10 all time")
	assert.Contains(t, page, "█", "a model with in-range tokens draws its strip")
	assert.Contains(t, page,
		"Projection coverage: 4 of 6 sessions have a usable projection. 1 missing, 1 unreadable.")

	// Ordered by in-range tokens: the busiest model comes first.
	assert.Less(t,
		strings.Index(page, "old/model: 300"),
		strings.Index(page, "small/model: 10"),
	)
}

// TestStatusPanelModelsRangeFiltersDaysAndKeepsAllTime pins the range: a day
// outside the window contributes nothing to the in-range total or the strip, while
// the all-time total keeps it, and a model with no in-range tokens still shows.
func TestStatusPanelModelsRangeFiltersDaysAndKeepsAllTime(t *testing.T) {
	t.Parallel()

	directory, metas := statusModelStore(t)
	model := statusModelsPageModel(t, directory, metas)

	model.Update(tea.KeyPressMsg{Text: "r"})
	require.Equal(t, statusStatsLast30, model.route.statusRange)

	page := statusPageText(model, statusTabModels)
	assert.Contains(t, page, "[Last 30 days]")
	assert.Contains(t, page, "demo/model: 120 in range · 120 all time")
	assert.Contains(t, page, "mixed/model: 50 in range · 250 all time",
		"the 40-day-old day leaves the range while the all-time total holds")
	assert.Contains(t, page, "small/model: 10 in range · 10 all time")
	assert.Contains(t, page, "old/model: 0 in range · 300 all time",
		"a model with no in-range tokens keeps its all-time total")

	// old/model sorts last and draws no strip, so the coverage line follows it.
	lines := strings.Split(page, "\n")
	index := -1
	for position, line := range lines {
		if strings.Contains(line, "old/model: 0 in range") {
			index = position
		}
	}
	require.GreaterOrEqual(t, index, 0)
	require.Less(t, index+1, len(lines))
	assert.Contains(t, lines[index+1], "Projection coverage",
		"a model without in-range tokens draws no strip")

	// The range key cycles the shared window on the Models page too.
	model.Update(tea.KeyPressMsg{Text: "r"})
	assert.Equal(t, statusStatsLast7, model.route.statusRange)
}

// TestStatusModelStripBoundedToWidth pins the strip geometry: one 2-column cell
// per day, bounded to the width by dropping the oldest days first.
func TestStatusModelStripBoundedToWidth(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	axis := make([]string, 0, 200)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	for index := range 200 {
		axis = append(axis, start.AddDate(0, 0, index).Format(usageProjectionDayKeyFormat))
	}
	row := statusModelHistory{ref: "demo/model", inRange: 200, days: map[string]int{}}
	for _, key := range axis {
		row.days[key] = 1
	}

	strip := model.statusModelStrip(row, axis, 40)
	assert.Equal(t, 38, ansi.StringWidth(strip), "the strip fits the width")
	assert.Equal(t, 19, ansi.StringWidth(strip)/statusModelsDayCellWidth, "the oldest days drop first")
	assert.Contains(t, strip, statusHeatmapLevels[4], "the busiest day is the top of the ramp")
}

// TestStatusModelStripSkipsAModelWithoutInRangeTokens pins the empty strip: a
// model with an all-time total but no tokens in range draws nothing.
func TestStatusModelStripSkipsAModelWithoutInRangeTokens(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	row := statusModelHistory{
		ref: "demo/model", inRange: 0, allTime: 300,
		days: map[string]int{"2026-10-06": 300},
	}

	assert.Empty(t, model.statusModelStrip(row, []string{"2026-10-06"}, 100))
}

// TestStatusPanelTabBarHasFiveTabs pins the fifth tab: the bar draws it, the hit
// map resolves it, and a click on it switches the page.
func TestStatusPanelTabBarHasFiveTabs(t *testing.T) {
	t.Parallel()

	require.Equal(t, 5, int(statusPanelTabCount))
	require.Len(t, statusPanelTabTitles, 5)

	model := fullscreenModel(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model.executeCommand(commandDescriptor{name: commandStatus})
	model.View()

	hits := model.route.statusHits
	require.Len(t, hits.tabs, 5, "every painted tab is addressable")
	assert.Equal(t, statusTabModels, hits.tabs[statusTabModels].tab)
	assert.Contains(t, model.statusRouteTabBar(100), "Models")

	command := model.handleMouse(tea.MouseClickMsg{
		X: hits.tabs[statusTabModels].start + 1, Y: hits.tabRow, Button: tea.MouseLeft,
	})
	require.Equal(t, statusTabModels, model.route.statusTab, "the fifth tab is clickable")
	driveModelCommands(t, model, command)
	assert.Contains(t, statusPageText(model, statusTabModels), "Models")
}

// TestStatusPanelTabBarFitsTheWidth pins that the fifth tab cannot push the header
// past the terminal: the bar is fitted to the panel width like the chrome rows
// around it, so the frame never exceeds the width it was laid out for.
func TestStatusPanelTabBarFitsTheWidth(t *testing.T) {
	t.Parallel()

	for _, width := range []int{80, 44, 40, 38, 34, 30} {
		model := fullscreenModel(t, newOverlayController(readyState()), true)
		model.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		model.executeCommand(commandDescriptor{name: commandStatus})

		rows := strings.Split(model.View().Content, "\n")
		row := -1
		for index, line := range rows {
			if strings.Contains(line, "Status") && strings.Contains(line, "Config") {
				row = index
			}
		}

		require.GreaterOrEqual(t, row, 0, "the tab row is drawn at width %d", width)
		assert.LessOrEqual(t, ansi.StringWidth(rows[row]), width,
			"the tab row fits the panel at width %d", width)
	}
}
