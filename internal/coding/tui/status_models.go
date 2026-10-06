//nolint:wsl_v5 // Model history aggregation and its strip stay adjacent.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding/session"
)

// statusModelsDayCellWidth is one day's column width in a model's strip, matching
// the activity grid's cells.
const statusModelsDayCellWidth = 2

// statusModelHistory is one model's projected tokens: the total in the selected
// range, the all-time total, and the in-range tokens per local day the strip
// draws. The two totals differ when the projection folds turns from days outside
// the range, and the all-time total always keeps them.
type statusModelHistory struct {
	ref     string
	inRange int
	allTime int
	days    map[string]int
}

// text names the two totals a model row reports.
func (row statusModelHistory) text() string {
	return fmt.Sprintf(
		"%s in range · %s all time",
		tokenCountText(row.inRange),
		tokenCountText(row.allTime),
	)
}

// statusModelHistories reduces the listed Sessions' usable projections to one row
// per model. A missing or unusable projection contributes nothing, and a day
// outside the selected range contributes to neither the in-range total nor the
// strip while the all-time total keeps every day it holds. Rows are ordered by
// in-range tokens, so a narrow window puts the models it covers first.
func statusModelHistories(
	sessions []session.Metadata,
	projections map[string]usageProjectionSnapshot,
	span statusStatsRange,
	now time.Time,
) []statusModelHistory {
	byRef := make(map[string]*statusModelHistory)
	for _, meta := range sessions {
		snapshot, ok := projections[meta.ID]
		if !ok || snapshot.read != usageProjectionUsable {
			continue
		}
		for ref, usage := range snapshot.projection.totals() {
			statusModelRow(byRef, ref).allTime += tokenUsageTotal(usage)
		}
		for day, value := range snapshot.projection.dailyTotals() {
			if !span.coversDay(day, now) {
				continue
			}
			for ref, usage := range value.Models {
				tokens := tokenUsageTotal(usage.usage())
				row := statusModelRow(byRef, ref)
				row.days[day] += tokens
				row.inRange += tokens
			}
		}
	}

	rows := make([]statusModelHistory, 0, len(byRef))
	for _, row := range byRef {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].inRange != rows[j].inRange {
			return rows[i].inRange > rows[j].inRange
		}
		if rows[i].allTime != rows[j].allTime {
			return rows[i].allTime > rows[j].allTime
		}

		return rows[i].ref < rows[j].ref
	})

	return rows
}

// statusModelRow returns a model's accumulator, creating it on first sight.
func statusModelRow(byRef map[string]*statusModelHistory, ref string) *statusModelHistory {
	row, ok := byRef[ref]
	if !ok {
		row = &statusModelHistory{ref: ref, days: map[string]int{}}
		byRef[ref] = row
	}

	return row
}

// statusModelStripAxis lists the local-day keys the strips cover, oldest first. A
// bounded range is the window ending today; the unbounded range runs from the
// earliest day any listed model recorded to today, so every strip shares one axis.
func statusModelStripAxis(
	rows []statusModelHistory,
	span statusStatsRange,
	now time.Time,
) []string {
	today := localDay(now)
	start := today
	if window := span.window(); window > 0 {
		start = today.AddDate(0, 0, -(window - 1))
	} else {
		for _, row := range rows {
			for day := range row.days {
				parsed, err := time.ParseInLocation(usageProjectionDayKeyFormat, day, time.Local)
				if err == nil && parsed.Before(start) {
					start = parsed
				}
			}
		}
	}
	keys := make([]string, 0, int(today.Sub(start).Hours()/24)+1)
	for day := start; !day.After(today); day = day.AddDate(0, 0, 1) {
		keys = append(keys, day.Format(usageProjectionDayKeyFormat))
	}

	return keys
}

// statusModelStrip renders one model's in-range days as a row of heatmap cells,
// bounded to the width by dropping the oldest days first. A model with no tokens
// in range renders nothing, so a narrow window never reads as "never used".
func (m *Model) statusModelStrip(row statusModelHistory, axis []string, width int) string {
	if row.inRange <= 0 || len(axis) == 0 {
		return ""
	}
	available := max(1, width-ansi.StringWidth(statusLabelIndent))
	fit := max(1, available/statusModelsDayCellWidth)
	if len(axis) > fit {
		axis = axis[len(axis)-fit:]
	}
	busiest := 0
	for _, day := range axis {
		busiest = max(busiest, row.days[day])
	}
	var builder strings.Builder
	for _, day := range axis {
		builder.WriteString(m.statusHeatmapCell(
			statusModelDayLevel(row.days[day], busiest), statusModelsDayCellWidth,
		))
	}

	return builder.String()
}

// statusModelDayLevel maps one day's tokens onto the activity grid's five-step
// ramp. The strip is per model, so a day shades against that model's busiest day
// on the axis: a low-volume model still shows its own shape instead of one flat
// step.
func statusModelDayLevel(tokens, busiest int) int {
	if tokens <= 0 || busiest <= 0 {
		return 0
	}
	level := (tokens*4 + busiest - 1) / busiest

	return min(max(level, 1), len(statusHeatmapLevels)-1)
}

// statusModelsLines is the Models page: the per-model history the projection
// carries, reduced over the same range selector the Stats page uses.
func (m *Model) statusModelsLines() []statusPageLine {
	lines := []statusPageLine{statusHeading("Models")}
	lines = append(lines, statusPageLine{
		kind: statusLineText, hot: statusHotRange,
		text: statusLabelIndent + m.statusStatsRangeRow(),
	})
	// The store read is asynchronous, so the page says it is reading rather than
	// reporting an empty history as if it had looked.
	if m.route.statusDataLoading {
		return append(lines, statusBlank(), statusText(
			statusLabelIndent+m.activityNotice("Reading the session store…"),
		))
	}
	now := m.statusNow()
	rows := statusModelHistories(
		m.route.statusSessions.Sessions, m.route.statusProjections, m.route.statusRange, now,
	)
	if len(rows) == 0 {
		lines = append(lines, statusBlank(), statusText(
			statusLabelIndent+"No model has a usable projection yet.",
		))
	} else {
		width := max(1, m.statusPageWidth())
		axis := statusModelStripAxis(rows, m.route.statusRange, now)
		lines = append(lines, statusBlank())
		for _, row := range rows {
			lines = append(lines, statusField(row.ref, row.text()))
			if strip := m.statusModelStrip(row, axis, width); strip != "" {
				lines = append(lines, statusText(statusLabelIndent+strip))
			}
		}
	}
	summary := aggregateStatusProjections(
		m.route.statusSessions.Sessions, m.route.statusProjections, m.controller.Config().Cost,
	)
	lines = append(lines, statusBlank(), statusText(
		statusLabelIndent+statusProjectionCoverageText(summary),
	))
	if m.route.statusSessions.Truncated {
		lines = append(lines, statusBlank(), statusText(
			statusLabelIndent+statusListingTruncationText(m.route.statusSessions),
		))
	}
	if m.route.statusDataErr != nil {
		lines = append(lines, statusBlank(), statusText(
			"Session history unavailable: "+safeError(m.route.statusDataErr),
		))
	}

	return lines
}
