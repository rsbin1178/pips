package background

import (
	"strconv"
	"strings"
	"time"
)

const (
	// userKilledNotice is appended when a kill's outcome never reached the model
	// through a Tool result, so the model knows not to relaunch the task.
	userKilledNotice = "This task was killed by the user — do not restart it.\n"

	// batchEventFallback labels an event whose wrapped text carries no usable
	// description.
	batchEventFallback = "event"
)

// MonitorCompletionText renders one monitor's single model-visible completion.
// The identity line prefers the description over the raw command, so the
// command is never echoed. The text points at the task-output Tool instead of
// inlining the log: the log is read by id, bounded, and the pane keeps its own
// tail for the watch page.
func MonitorCompletionText(completion Completion) string {
	var builder strings.Builder

	builder.WriteString("Monitor \"")
	builder.WriteString(completion.Snapshot.ID)
	builder.WriteString("\" ended: [monitor ended: ")
	builder.WriteString(monitorEndReason(completion))
	builder.WriteString("].\n")
	builder.WriteString(monitorIdentityLine(completion.Snapshot))
	builder.WriteString("\nUse ")
	builder.WriteString(TaskOutputToolName)
	builder.WriteString("(\"")
	builder.WriteString(completion.Snapshot.ID)
	builder.WriteString("\") for full output.\n")

	if completion.Killed && !completion.KillResultDelivered {
		builder.WriteString(userKilledNotice)
	}

	return builder.String()
}

// monitorEndReason classifies how the watched process stopped. A signaled run
// names its signal; a run that exited reports its code; anything else — a kill,
// an operation deadline, or an output limit — reports that the watch ended.
func monitorEndReason(completion Completion) string {
	if signal := completion.Snapshot.Signal; signal != "" {
		return "killed by signal " + signal
	}

	if code := completion.Snapshot.ExitCode; code >= 0 {
		return "exited (code " + strconv.Itoa(code) + ")"
	}

	return "ended"
}

// ShellCompletionText renders one shell task's single model-visible completion:
// the status, the identity line, and the pointer at the task-output Tool. A
// shell call carries no description, and the command is never echoed.
func ShellCompletionText(completion Completion) string {
	var builder strings.Builder

	builder.WriteString("Background task \"")
	builder.WriteString(completion.Snapshot.ID)
	builder.WriteString("\" completed (")
	builder.WriteString(shellEndReason(completion))
	builder.WriteString(").\n")
	builder.WriteString("Duration: ")
	builder.WriteString(formatSeconds(completion.Snapshot.End.Sub(completion.Snapshot.Start)))
	builder.WriteString("\nUse ")
	builder.WriteString(TaskOutputToolName)
	builder.WriteString("(\"")
	builder.WriteString(completion.Snapshot.ID)
	builder.WriteString("\") to see the full output.\n")

	if completion.Killed && !completion.KillResultDelivered {
		builder.WriteString(userKilledNotice)
	}

	return builder.String()
}

// shellEndReason classifies how the command stopped: a signaled run names its
// signal, a run that exited reports its code, and anything else reports the
// code as unknown.
func shellEndReason(completion Completion) string {
	if signal := completion.Snapshot.Signal; signal != "" {
		return "terminated by signal " + signal
	}

	if code := completion.Snapshot.ExitCode; code >= 0 {
		return "exit code: " + strconv.Itoa(code)
	}

	return "exit code: unknown"
}

// monitorIdentityLine renders the description-preferred identity line.
func monitorIdentityLine(snapshot Snapshot) string {
	duration := formatSeconds(snapshot.End.Sub(snapshot.Start))

	if description := strings.TrimSpace(snapshot.Description); description != "" {
		return "Description: " + description + " | Duration: " + duration
	}

	return "Duration: " + duration
}

// MonitorRunningWarning renders the note a monitor start carries while other
// background tasks are still running. A running task's elapsed time is measured
// against now.
func MonitorRunningWarning(running []Snapshot, now time.Time, killToolName string) string {
	if len(running) == 0 {
		return ""
	}

	if killToolName == "" {
		killToolName = KillTaskToolName
	}

	var builder strings.Builder

	builder.WriteString("Note: ")
	builder.WriteString(strconv.Itoa(len(running)))

	if len(running) == 1 {
		builder.WriteString(" other background task is still running:\n")
	} else {
		builder.WriteString(" other background tasks are still running:\n")
	}

	for _, snapshot := range running {
		builder.WriteString("- \"")
		builder.WriteString(snapshot.ID)
		builder.WriteString("\" (running for ")
		builder.WriteString(strconv.FormatFloat(elapsedSeconds(snapshot, now), 'f', 0, 64))
		builder.WriteString("s)")

		if description := strings.TrimSpace(snapshot.Description); description != "" {
			builder.WriteString(": ")
			builder.WriteString(description)
		}

		builder.WriteString("\n")
	}

	builder.WriteString("Consider killing duplicate tasks with ")
	builder.WriteString(killToolName)
	builder.WriteString(" before launching new ones.")

	return builder.String()
}

// elapsedSeconds reports how long one task has run, measured to its end when it
// has ended and to now while it is still running.
func elapsedSeconds(snapshot Snapshot, now time.Time) float64 {
	end := snapshot.End
	if end.IsZero() {
		end = now
	}

	return end.Sub(snapshot.Start).Seconds()
}

// MonitorBatchText renders drained monitor events as one model-visible batch.
// A single event uses the single-event shape; more than one uses the counted,
// per-monitor shape. It returns "" when there is nothing to deliver.
func MonitorBatchText(events []Event, toolHint string) string {
	switch len(events) {
	case 0:
		return ""
	case 1:
		return singleEventText(events[0])
	default:
		return multiEventText(events, toolHint)
	}
}

func singleEventText(event Event) string {
	label, inner, ok := splitWrappedEvent(event.Wrapped)
	if !ok {
		label, inner = batchEventFallback, event.Wrapped
	} else if label == "" {
		label = batchEventFallback
	}

	var builder strings.Builder

	builder.WriteString("<monitor-event task_id=\"")
	builder.WriteString(event.TaskID)
	builder.WriteString("\">\n[")
	builder.WriteString(sanitizeDescription(label))
	builder.WriteString("] ")
	builder.WriteString(inner)
	builder.WriteString("\n</monitor-event>")

	return builder.String()
}

func multiEventText(events []Event, toolHint string) string {
	if toolHint == "" {
		toolHint = KillTaskToolName
	}

	order := make([]string, 0, len(events))
	groups := make(map[string][]Event, len(events))

	for _, event := range events {
		if _, seen := groups[event.TaskID]; !seen {
			order = append(order, event.TaskID)
		}

		groups[event.TaskID] = append(groups[event.TaskID], event)
	}

	var builder strings.Builder

	builder.WriteString(strconv.Itoa(len(events)))
	builder.WriteString(" monitor events from ")
	builder.WriteString(strconv.Itoa(len(order)))

	if len(order) == 1 {
		builder.WriteString(" monitor")
	} else {
		builder.WriteString(" monitors")
	}

	builder.WriteString(" (use ")
	builder.WriteString(toolHint)
	builder.WriteString(" to identify each monitor):")

	for _, taskID := range order {
		builder.WriteString("\n\n<monitor description=\"")
		builder.WriteString(sanitizeDescription(groupDescription(groups[taskID])))
		builder.WriteString("\" task_id=\"")
		builder.WriteString(taskID)
		builder.WriteString("\">")

		for index, event := range groups[taskID] {
			builder.WriteString("\n[")
			builder.WriteString(strconv.Itoa(index + 1))
			builder.WriteString("] ")
			builder.WriteString(eventInner(event))
		}

		builder.WriteString("\n</monitor>")
	}

	return builder.String()
}

// groupDescription is the first description in one monitor's group that parses,
// or the fallback label.
func groupDescription(events []Event) string {
	for _, event := range events {
		if description, _, ok := splitWrappedEvent(event.Wrapped); ok && description != "" {
			return description
		}
	}

	return batchEventFallback
}

func eventInner(event Event) string {
	if _, inner, ok := splitWrappedEvent(event.Wrapped); ok {
		return inner
	}

	return event.Wrapped
}

// formatSeconds renders one duration in tenths of a second.
func formatSeconds(duration time.Duration) string {
	return strconv.FormatFloat(duration.Seconds(), 'f', 1, 64) + "s"
}
