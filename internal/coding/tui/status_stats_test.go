//nolint:wsl_v5 // Statistics fixtures and their assertions stay adjacent.
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
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionActivitySummarisesStreaksAndRanges(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 6, 15, 30, 0, 0, time.Local)
	activity := newSessionActivity([]session.Metadata{
		{CreatedAt: now},
		{CreatedAt: now.AddDate(0, 0, -1)},
		{CreatedAt: now.AddDate(0, 0, -1).Add(2 * time.Hour)},
		{CreatedAt: now.AddDate(0, 0, -5)},
		{CreatedAt: now.AddDate(0, 0, -40)},
		{},
	})
	require.Equal(t, 5, activity.total, "a Session without a timestamp is not dated")

	all := activity.summary(statusStatsAllTime, now)
	assert.Equal(t, 5, all.sessions)
	assert.Equal(t, 4, all.activeDays)
	assert.Equal(t, 2, all.currentStreak)
	assert.Equal(t, 2, all.longestStreak)
	assert.Equal(t, now.AddDate(0, 0, -40).Format("Jan 2, 2006"), all.firstDay)
	assert.Equal(t, now.AddDate(0, 0, -1).Format("Jan 2"), all.mostActiveDay)
	assert.Equal(t, 2, all.mostActiveCount)
	assert.Equal(t, 41, all.windowDays)

	month := activity.summary(statusStatsLast30, now)
	assert.Equal(t, 4, month.sessions, "the 40-day-old Session leaves the window")
	assert.Equal(t, 3, month.activeDays)
	assert.Equal(t, 30, month.windowDays)

	week := activity.summary(statusStatsLast7, now)
	assert.Equal(t, 4, week.sessions)
	assert.Equal(t, 7, week.windowDays)
	assert.Equal(t, 2, week.currentStreak)
}

// TestSessionActivityStreakSurvivesAnIdleToday pins the streak rule: a day that
// has not been active yet does not end the run that finished yesterday.
func TestSessionActivityStreakSurvivesAnIdleToday(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local)
	activity := newSessionActivity([]session.Metadata{
		{CreatedAt: now.AddDate(0, 0, -1)},
		{CreatedAt: now.AddDate(0, 0, -2)},
		{CreatedAt: now.AddDate(0, 0, -8)},
	})
	summary := activity.summary(statusStatsLast7, now)

	assert.Equal(t, 2, summary.currentStreak)
	assert.Equal(t, 2, summary.longestStreak)
	assert.Equal(t, 2, summary.activeDays)
}

func TestSessionActivityLevelsRampByCount(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local)
	metas := make([]session.Metadata, 0, 32)
	for count := range 8 {
		for range max(1, count) {
			metas = append(metas, session.Metadata{CreatedAt: now.AddDate(0, 0, -count)})
		}
	}
	activity := newSessionActivity(metas)

	assert.Equal(t, 0, activity.level(now.AddDate(0, 0, -30)), "an idle day is empty")
	assert.Equal(t, 1, activity.level(now), "one Session is the first step")
	assert.Equal(t, 2, activity.level(now.AddDate(0, 0, -2)))
	assert.Equal(t, 3, activity.level(now.AddDate(0, 0, -4)))
	assert.Equal(t, 4, activity.level(now.AddDate(0, 0, -7)))
}

// TestStatusHeatmapGridKeepsFutureDaysBlank pins the grid shape: a month row plus
// one row per weekday, with the days after today left empty.
func TestStatusHeatmapGridKeepsFutureDaysBlank(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local) // a Tuesday
	activity := newSessionActivity([]session.Metadata{{CreatedAt: now}})
	model := readyModelWithController(t, newOverlayController(readyState()), true)

	rows := model.statusHeatmapRows(activity, now, 120)
	require.Len(t, rows, 8, "a month row plus one row per weekday")
	for _, row := range rows {
		assert.LessOrEqual(t, ansi.StringWidth(row), 120)
	}
	assert.Contains(t, rows[0], "Oct", "the month row labels the current month")

	tuesday := rows[1+1]
	wednesday := rows[1+2]
	assert.True(t, strings.HasSuffix(tuesday, statusHeatmapLevels[1]),
		"today's cell carries activity: %q", tuesday)
	assert.Equal(t, ansi.StringWidth(tuesday)-2, ansi.StringWidth(wednesday),
		"a day after today leaves its cell blank: %q", wednesday)
}

// statusProjectionStore writes a mixed store: two usable projections, one absent
// and one corrupt, and returns the directory and the listed Sessions.
func statusProjectionStore(t *testing.T) (string, []session.Metadata) {
	t.Helper()

	directory := t.TempDir()
	// The cached and reasoning classes are present so the reduction cannot count
	// them twice: input includes the cached tokens and output includes reasoning.
	require.True(t, writeUsageProjection(
		directory, "s-1", "i-1", 60_000,
		map[string]coding.TokenUsage{"demo/model": {
			InputTokens: 100, CachedInputTokens: 40, OutputTokens: 20, ReasoningTokens: 5,
		}},
		time.Now(),
	).wrote)
	require.True(t, writeUsageProjection(
		directory, "s-2", "i-1", 30_000,
		map[string]coding.TokenUsage{"other/model": {InputTokens: 300}},
		time.Now(),
	).wrote)

	corrupt := filepath.Join(directory, "s-4")
	require.NoError(t, os.MkdirAll(corrupt, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(corrupt, usageProjectionFile), []byte("{not json"), 0o600))

	return directory, []session.Metadata{{ID: "s-1"}, {ID: "s-2"}, {ID: "s-3"}, {ID: "s-4"}}
}

// TestStatusAggregateProjectionsReducesEveryListedSession pins the all-time
// reduction: total tokens, the favourite model by tokens, the longest and average
// active time, and the coverage of a store mixing usable, missing and corrupt
// projections. A bad projection contributes nothing.
func TestStatusAggregateProjectionsReducesEveryListedSession(t *testing.T) {
	t.Parallel()

	directory, metas := statusProjectionStore(t)
	projections := make(map[string]usageProjectionSnapshot, len(metas))
	for _, meta := range metas {
		projection, read := readUsageProjectionStatus(directory, meta.ID)
		projections[meta.ID] = usageProjectionSnapshot{projection: projection, read: read}
	}

	summary := aggregateStatusProjections(metas, projections, config.CostConfig{})
	assert.Equal(t, 4, summary.listed)
	assert.Equal(t, 2, summary.usable)
	assert.Equal(t, 1, summary.missing)
	assert.Equal(t, 1, summary.unreadable)
	assert.Equal(t, 420, summary.totalTokens)
	assert.Equal(t, "other/model", summary.favouriteModel)
	assert.Equal(t, 300, summary.favouriteTokens)
	assert.Equal(t, int64(60_000), summary.longestMillis)
	assert.Equal(t, int64(45_000), summary.averageMillis)
	assert.Equal(t, 2, summary.activeSessions)
}

// TestStatusPanelStatsAllTimeRendersTheProjections pins the rendered all-time
// block over the mixed store, while the range rows keep responding to the key.
func TestStatusPanelStatsAllTimeRendersTheProjections(t *testing.T) {
	t.Parallel()

	directory, metas := statusProjectionStore(t)
	now := time.Now()
	controller := newOverlayController(readyState())
	for _, meta := range metas {
		meta.CreatedAt = now
		controller.sessions = append(controller.sessions, meta)
	}
	model := readyModelWithController(t, controller, true)
	model.options.SessionsDirectory = directory
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	model.executeCommand(commandDescriptor{name: commandStatus})
	driveModelCommands(t, model, model.selectStatusTab(statusTabStats))

	page := statusPageText(model, statusTabStats)
	assert.Contains(t, page, "All time")
	assert.Contains(t, page, "Total tokens: 420")
	assert.Contains(t, page, "Favourite model: other/model · 300 tokens")
	assert.Contains(t, page, "Longest session: 1m active")
	assert.Contains(t, page, "Average session: 45s active over 2 sessions")
	assert.Contains(t, page,
		"Projection coverage: 2 of 4 sessions have a usable projection. 1 missing, 1 unreadable.")

	// The range rows stay range-sensitive: the block is labelled all time, and the
	// key still cycles the window.
	assert.Contains(t, page, "Sessions: 4")
	model.Update(tea.KeyPressMsg{Text: "r"})
	assert.Equal(t, statusStatsLast30, model.route.statusRange)
	assert.Contains(t, statusPageText(model, statusTabStats), "All time")
}
