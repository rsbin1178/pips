//nolint:wsl_v5 // Statistics fixtures and their assertions stay adjacent.
package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
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
