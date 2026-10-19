//nolint:wsl_v5 // The standalone checklist page keeps its route and renderer adjacent.
package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
)

// openTodoDetailRoute opens the whole todo list on the shared detail route. The
// status line carries the compact progress; this page is the inspectable
// surface, so it keeps the plan's own order and includes every step.
func (m *Model) openTodoDetailRoute() tea.Cmd {
	if len(m.state.Tasks.Items) == 0 {
		return m.setStatusNotice("No todo items yet.")
	}

	return m.openToolDetailRoute(newTodoDetailView(m.state.Tasks))
}

// newTodoDetailView renders the todo checklist the model wrote. It reads
// the same snapshot the status line and the timeline project, so no surface can
// show a different plan.
func newTodoDetailView(snapshot tasklist.Snapshot) toolDetailView {
	view := toolDetailView{title: "Todo details", todo: true}
	if len(snapshot.Items) == 0 {
		view.rows = append(view.rows, detailNoteRow("No todo items."))

		return view
	}

	counts := strconv.Itoa(snapshot.Completed) + "/" + strconv.Itoa(snapshot.Total) + " completed"
	if snapshot.Cancelled > 0 {
		counts += " · " + strconv.Itoa(snapshot.Cancelled) + " cancelled"
	}
	view.rows = append(view.rows, detailNoteRow(counts), detailSection("Plan"))
	for _, item := range snapshot.Items {
		view.rows = append(view.rows, toolDetailRow{
			tone: detailToneTask(item.Status), indent: 1,
			text: taskListGlyph(item.Status) + " " + taskListItemLabel(item),
		})
	}

	return view
}
