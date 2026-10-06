//nolint:wsl_v5 // Statistics bucketing and heatmap composition stay adjacent.
package tui

import (
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding/session"
)

// statusStatsRange selects the window the statistics numbers describe. The
// heatmap always covers the whole year, as the reference panel does; the range
// narrows the numbers under it.
type statusStatsRange uint8

const (
	statusStatsAllTime statusStatsRange = iota
	statusStatsLast30
	statusStatsLast7
	statusStatsRangeCount
)

func (value statusStatsRange) title() string {
	switch value {
	case statusStatsLast7:
		return "Last 7 days"
	case statusStatsLast30:
		return "Last 30 days"
	default:
		return "All time"
	}
}

// next cycles the range the way the reference panel's key does.
func (value statusStatsRange) next() statusStatsRange {
	return (value + 1) % statusStatsRangeCount
}

// window is the range length in days, or zero for the unbounded range.
func (value statusStatsRange) window() int {
	switch value {
	case statusStatsLast7:
		return 7
	case statusStatsLast30:
		return 30
	default:
		return 0
	}
}

// statusHeatmapLevels is the five-step density ramp the activity grid shades a
// day with: one glyph per step rather than blended background colors, so the grid
// keeps its meaning in every theme and under NO_COLOR.
var statusHeatmapLevels = [...]string{"·", "░", "▒", "▓", "█"}

// statusHeatmapMonths are the three-letter month names the grid annotates its
// columns with.
var statusHeatmapMonths = [...]string{
	"Jan", "Feb", "Mar", "Apr", "May", "Jun",
	"Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
}

// statusHeatmapWeekdays labels the Monday, Wednesday and Friday rows, which is
// what the reference grid labels.
var statusHeatmapWeekdays = [...]string{"Mon", "", "Wed", "", "Fri", "", ""}

// sessionActivity buckets durable Sessions by the local day they were created on.
// pips reduces the store's headers, so the aggregate carries activity and counts
// rather than the per-model token totals the reference derives by parsing every
// transcript.
type sessionActivity struct {
	counts map[time.Time]int
	days   []time.Time
	total  int
	first  time.Time
	last   time.Time
}

// newSessionActivity counts Sessions per local calendar day. A Session without a
// timestamp is ignored rather than dated to the epoch.
func newSessionActivity(metas []session.Metadata) sessionActivity {
	activity := sessionActivity{counts: make(map[time.Time]int, len(metas))}
	for _, meta := range metas {
		if meta.CreatedAt.IsZero() {
			continue
		}
		activity.counts[localDay(meta.CreatedAt)]++
		activity.total++
	}
	activity.days = make([]time.Time, 0, len(activity.counts))
	for day := range activity.counts {
		activity.days = append(activity.days, day)
	}
	sort.Slice(activity.days, func(i, j int) bool {
		return activity.days[i].Before(activity.days[j])
	})
	if len(activity.days) > 0 {
		activity.first = activity.days[0]
		activity.last = activity.days[len(activity.days)-1]
	}

	return activity
}

// localDay truncates a timestamp to its local calendar day.
func localDay(value time.Time) time.Time {
	local := value.In(time.Local)

	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
}

// level maps a day's Session count onto the five heatmap steps. It accepts any
// time on that day, not only its midnight.
func (activity sessionActivity) level(day time.Time) int {
	switch count := activity.counts[localDay(day)]; {
	case count <= 0:
		return 0
	case count == 1:
		return 1
	case count <= 3:
		return 2
	case count <= 6:
		return 3
	default:
		return 4
	}
}

// sessionStatsSummary is one range of the activity grid reduced to numbers.
type sessionStatsSummary struct {
	span            statusStatsRange
	sessions        int
	activeDays      int
	windowDays      int
	currentStreak   int
	longestStreak   int
	mostActiveDay   string
	mostActiveCount int
	firstDay        string
}

// summary reduces the activity to the numbers one range reports.
func (activity sessionActivity) summary(value statusStatsRange, now time.Time) sessionStatsSummary {
	today := localDay(now)
	start := today
	switch days := value.window(); {
	case days > 0:
		start = today.AddDate(0, 0, -(days - 1))
	case !activity.first.IsZero():
		start = activity.first
	}
	summary := sessionStatsSummary{
		span:       value,
		windowDays: int(today.Sub(start).Hours()/24) + 1,
	}
	streak := 0
	for day := start; !day.After(today); day = day.AddDate(0, 0, 1) {
		count := activity.counts[day]
		if count == 0 {
			streak = 0

			continue
		}
		summary.sessions += count
		summary.activeDays++
		streak++
		summary.longestStreak = max(summary.longestStreak, streak)
		if summary.firstDay == "" {
			summary.firstDay = day.Format("Jan 2, 2006")
		}
		if count > summary.mostActiveCount {
			summary.mostActiveCount = count
			summary.mostActiveDay = day.Format("Jan 2")
		}
	}
	// A day that has not been active yet does not break the current streak: it
	// runs back from today, or from the most recent active day before it.
	probe := today
	if activity.counts[probe] == 0 {
		probe = probe.AddDate(0, 0, -1)
	}
	for !probe.Before(start) && activity.counts[probe] > 0 {
		summary.currentStreak++
		probe = probe.AddDate(0, 0, -1)
	}

	return summary
}

// statusHeatmapRows renders the GitHub-style activity grid: one column per week
// with Monday-to-Sunday rows and month labels above. The grid keeps as many weeks
// as the width allows.
func (m *Model) statusHeatmapRows(activity sessionActivity, now time.Time, width int) []string {
	const (
		labelWidth  = 4
		weekdayRows = 7
		maxWeeks    = 53
		cellWidth   = 2
	)
	today := localDay(now)
	firstOfWeek := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % weekdayRows))
	// Two columns of slack let the last month label finish inside the width.
	weeks := min(maxWeeks, max(4, (max(1, width)-labelWidth-2)/cellWidth))
	first := firstOfWeek.AddDate(0, 0, -weekdayRows*(weeks-1))

	rows := make([]string, 0, weekdayRows+1)
	rows = append(rows, m.statusHeatmapMonthRow(first, weeks, labelWidth, cellWidth))
	for row := range weekdayRows {
		rows = append(rows, m.statusHeatmapWeekRow(
			activity, first, row, weeks, today, labelWidth, cellWidth,
		))
	}

	return rows
}

// statusHeatmapMonthRow places the three-letter month name above each column that
// starts a new month. Labels may run past their own column because the following
// cell is a space unless another month starts there.
func (m *Model) statusHeatmapMonthRow(first time.Time, weeks, labelWidth, cellWidth int) string {
	cells := make([]rune, labelWidth+weeks*cellWidth)
	for index := range cells {
		cells[index] = ' '
	}
	for week := range weeks {
		day := first.AddDate(0, 0, week*7)
		if week != 0 && day.Month() == day.AddDate(0, 0, -7).Month() {
			continue
		}
		start := labelWidth + week*cellWidth
		for offset, character := range statusHeatmapMonths[day.Month()-1] {
			if start+offset < len(cells) {
				cells[start+offset] = character
			}
		}
	}

	return strings.TrimRight(string(cells), " ")
}

// statusHeatmapWeekRow renders one weekday row of the grid. A day that has not
// happened yet stays blank so the grid never invents activity.
func (m *Model) statusHeatmapWeekRow(
	activity sessionActivity,
	first time.Time,
	row, weeks int,
	today time.Time,
	labelWidth, cellWidth int,
) string {
	var builder strings.Builder
	builder.WriteString(padCells(statusHeatmapWeekdays[row], labelWidth))
	for week := range weeks {
		day := first.AddDate(0, 0, week*7+row)
		if day.After(today) {
			builder.WriteString(strings.Repeat(" ", cellWidth))

			continue
		}
		builder.WriteString(m.statusHeatmapCell(activity.level(day), cellWidth))
	}

	return strings.TrimRight(builder.String(), " ")
}

// statusHeatmapCell renders one day at a fixed cell width. The ramp keeps one hue
// and lets the glyph carry the intensity, so no theme has to supply a heat scale.
func (m *Model) statusHeatmapCell(level, cellWidth int) string {
	glyph := statusHeatmapLevels[min(max(level, 0), len(statusHeatmapLevels)-1)]
	if m.options.NoColor {
		return padCells(glyph, cellWidth)
	}
	palette := paletteFor(m.theme)
	style := lipgloss.NewStyle().Foreground(palette.separator)
	if level > 0 {
		style = lipgloss.NewStyle().Foreground(palette.active)
	}

	return padCells(style.Render(glyph), cellWidth)
}

// statusHeatmapLegend pairs the lowest and highest steps with their words.
func (m *Model) statusHeatmapLegend() string {
	cells := make([]string, 0, len(statusHeatmapLevels))
	for level := range statusHeatmapLevels {
		cells = append(cells, m.statusHeatmapCell(level, 1))
	}

	return "Less " + strings.Join(cells, "") + " More"
}

// padCells pads a rendered cell to an exact column width.
func padCells(value string, width int) string {
	text := ansi.Truncate(value, width, "")
	if padding := width - ansi.StringWidth(text); padding > 0 {
		text += strings.Repeat(" ", padding)
	}

	return text
}
