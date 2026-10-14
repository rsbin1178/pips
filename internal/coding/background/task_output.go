package background

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Task-output status words. They are the reference's closed vocabulary, chosen
// in this order: a kill, then the task's own deadline, then a clean exit, then
// anything else. `initializing` and `cancelled` are the two words a delegated
// child adds: a child that has a durable lifecycle but no live run is
// initializing, and a cancelled or interrupted child is cancelled.
const (
	StatusInitializing = "initializing"
	StatusRunning      = "running"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
	StatusCancelled    = "cancelled"
	StatusTimedOut     = "timed_out"
	StatusNotFound     = "not_found"
)

// Subject names what one output record describes. A background task and a
// delegated child share the record shape, so one renderer serves both; the
// subject is what lets the wait hint address the right thing, because only a
// child can be told to stop. The zero value reads as a task.
type Subject string

// Supported output subjects.
const (
	SubjectTask     Subject = "task"
	SubjectSubagent Subject = "subagent"
)

const (
	// taskOutputBytes bounds one task's model-visible output dump. It is the
	// reference's default tool-output bound.
	taskOutputBytes = 40_000
	// previewBytes is how much of an over-bound log the dump keeps, cut at a
	// rune boundary.
	previewBytes = 2_000
	// readToolName is the Tool the truncation footer names as the way to the
	// rest of a long log.
	readToolName = "read"
)

// WaitOutcome classifies how a blocking read ended.
type WaitOutcome string

// Supported wait outcomes.
const (
	// WaitOutcomeCompleted means the wait condition was met.
	WaitOutcomeCompleted WaitOutcome = "completed"
	// WaitOutcomeDeadline means the budget elapsed first.
	WaitOutcomeDeadline WaitOutcome = "deadline"
	// WaitOutcomeCanceled means the caller's context ended first.
	WaitOutcomeCanceled WaitOutcome = "canceled"
)

// WaitHint classifies why a running record's wait returned, so the record can
// carry the reference's own wait words.
type WaitHint uint8

// Supported wait hints.
const (
	// WaitHintPoll is a snapshot with no wait requested.
	WaitHintPoll WaitHint = iota
	// WaitHintElapsed is a wait whose budget ran out.
	WaitHintElapsed
	// WaitHintReturnedEarly is a wait that returned because another id finished.
	WaitHintReturnedEarly
)

// TaskOutput is one task's model-visible output record. Its fields are the
// reference's, split the way every coding Tool splits its output: the record
// rides the result header, and its body is the reference's own rendering.
type TaskOutput struct {
	// Found is false for an id the registry does not know.
	Found bool
	// Subject is what the record describes, so the wait hint addresses a task
	// or a child in its own words. The zero value reads as a task.
	Subject        Subject
	TaskID         string
	Command        string
	Status         string
	ExitCode       *int
	Started        time.Time
	Ended          time.Time
	DurationSecs   float64
	Output         string
	OutputFile     string
	Truncated      bool
	TruncationHint string
	RawOutputBytes int64
}

// Live reports whether the record's subject is still working, so a wait hint
// belongs on its body and a terminal status word does not.
func (r TaskOutput) Live() bool {
	return r.Status == StatusRunning || r.Status == StatusInitializing
}

// NotFoundTaskOutput is the record an unknown id yields. It carries the
// reference's `not_found` status and sentence, so a multi read renders the
// unknown entry beside the known ones instead of an empty one.
func NotFoundTaskOutput(id string) TaskOutput {
	return TaskOutput{
		TaskID: id,
		Status: StatusNotFound,
		Output: "Task " + id + " not found.",
	}
}

// taskOutputFromSnapshot projects one task into its output record. now measures
// a still-running task's elapsed time, as the reference does.
func taskOutputFromSnapshot(
	snapshot Snapshot,
	killed, timedOut bool,
	path string,
	now time.Time,
) TaskOutput {
	record := TaskOutput{
		Found:      true,
		Subject:    SubjectTask,
		TaskID:     snapshot.ID,
		Command:    snapshot.Command,
		Status:     taskOutputStatus(snapshot, killed, timedOut),
		Started:    snapshot.Start,
		Ended:      snapshot.End,
		OutputFile: path,
	}

	if snapshot.Completed && snapshot.ExitCode >= 0 {
		code := snapshot.ExitCode
		record.ExitCode = &code
	}

	record.DurationSecs = taskOutputDuration(snapshot, now)

	record.Output, record.Truncated, record.RawOutputBytes = readTaskOutputDump(path)
	if record.Truncated {
		record.TruncationHint = truncationHint()
	}

	return record
}

// taskOutputStatus chooses the record's status word. A kill wins over the
// deadline, and a task that has not ended is running whatever its exit code.
func taskOutputStatus(snapshot Snapshot, killed, timedOut bool) string {
	switch {
	case !snapshot.Completed:
		return StatusRunning
	case killed:
		return StatusCancelled
	case timedOut:
		return StatusTimedOut
	case snapshot.ExitCode == 0:
		return StatusCompleted
	default:
		return StatusFailed
	}
}

// taskOutputDuration reports how long the task has run, measured to its end
// when it has ended and to now while it is still running.
func taskOutputDuration(snapshot Snapshot, now time.Time) float64 {
	end := snapshot.End
	if end.IsZero() {
		end = now
	}

	return end.Sub(snapshot.Start).Seconds()
}

// readTaskOutputDump reads a bounded, sanitized projection of a task's log. The
// whole log is the dump while it fits; past the bound the dump is the first
// previewBytes cut at a rune boundary plus a footer naming the real total and
// the read Tool. A missing or unreadable file reads as empty.
func readTaskOutputDump(path string) (string, bool, int64) {
	if path == "" {
		return "", false, 0
	}

	//nolint:gosec // The path is a task's own output file inside the session directory.
	file, err := os.Open(path)
	if err != nil {
		return "", false, 0
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return "", false, 0
	}

	total := info.Size()

	if total <= taskOutputBytes {
		whole := make([]byte, total)

		count, readErr := io.ReadFull(file, whole)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return "", false, total
		}

		return sanitizeText(string(whole[:count])), false, total
	}

	preview := make([]byte, previewBytes)

	count, readErr := io.ReadFull(file, preview)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return "", false, total
	}

	footer := truncationFooter(total, path)

	return sanitizeText(string(runeBoundary(preview[:count]))) + "\n\n" + footer, true, total
}

// truncationFooter is the line that reports the real total and where the rest
// of the log lives.
func truncationFooter(total int64, path string) string {
	return "[Output truncated - " + strconv.FormatInt(total, 10) +
		" bytes total. Use " + readToolName + " on " + path + " for full content]"
}

// BoundTaskOutputText bounds an in-memory body at the same taskOutputBytes a
// task log is bounded at, so a delegated child's result can never grow a Tool
// result without limit. Past the bound the dump is the first previewBytes cut at
// a rune boundary plus the path-free footer: a child's result has no file to
// point at, and a hint that promised one would be a dead pointer. The returned
// count is the body's real length, before the bound applied.
func BoundTaskOutputText(text string) (string, bool, int64) {
	total := int64(len(text))
	if total <= taskOutputBytes {
		return text, false, total
	}

	preview := runeBoundary([]byte(text)[:previewBytes])

	return string(preview) + "\n\n" + truncationTotalFooter(total), true, total
}

// truncationTotalFooter is the path-free truncation footer a body without a
// file behind it carries.
func truncationTotalFooter(total int64) string {
	return "[Output truncated - " + strconv.FormatInt(total, 10) + " bytes total]"
}

// TotalTruncationHint is the short marker a truncated record without a file
// behind it carries, so its structured hint reports the real length instead of
// naming a Tool and a path that do not exist.
func TotalTruncationHint(total int64) string {
	return "[truncated - " + strconv.FormatInt(total, 10) + " bytes total]"
}

// truncationHint is the short marker a truncated record carries.
func truncationHint() string {
	return "[truncated - use " + readToolName + " on output_file for full content]"
}

// runeBoundary drops a trailing partial rune so the preview stays valid UTF-8.
func runeBoundary(value []byte) []byte {
	for len(value) > 0 {
		r, size := utf8.DecodeLastRune(value)
		if r != utf8.RuneError || size > 1 {
			break
		}

		value = value[:len(value)-1]
	}

	return value
}

// RenderTaskOutputBody renders one record's model-facing body: the reference's
// `=== Task ===` block, then its output, then the truncation hint.
func RenderTaskOutputBody(record TaskOutput) string {
	lines := []string{
		"=== Task " + record.TaskID + " ===",
		"Command: " + record.Command,
		"Status: " + record.Status,
		"Duration: " + formatDurationSeconds(record.DurationSecs),
	}

	if record.ExitCode != nil {
		lines = append(lines, "Exit Code: "+strconv.Itoa(*record.ExitCode))
	}

	if record.OutputFile != "" {
		lines = append(lines, "Output File: "+record.OutputFile)
	}

	lines = append(lines, "", "=== Output ===")

	switch {
	case record.Output != "":
		lines = append(lines, record.Output)
	case record.Live():
		lines = append(lines, "(no output yet)")
	default:
		lines = append(lines, "(no output)")
	}

	if record.Truncated {
		lines = append(lines, record.TruncationHint)
	}

	return strings.Join(lines, "\n")
}

// RenderMultiTaskOutputBody renders several records as the reference's multi
// shape: one `=== Multi-wait (<mode>) ===` header, one entry per record, and the
// completed-count summary.
func RenderMultiTaskOutputBody(mode string, records []TaskOutput, summary string) string {
	lines := []string{"=== Multi-wait (" + mode + ") ==="}

	for _, record := range records {
		entry := "--- Task " + record.TaskID + " [" + record.Status + "] ---\n" +
			"Command: " + record.Command + "\n" +
			"Duration: " + formatDurationSeconds(record.DurationSecs)

		if record.ExitCode != nil {
			entry += "\nExit Code: " + strconv.Itoa(*record.ExitCode)
		}

		if record.Output != "" {
			entry += "\n" + record.Output
		}

		lines = append(lines, entry)
	}

	lines = append(lines, "\n"+summary)

	return strings.Join(lines, "\n")
}

// ApplyWaitHint appends the reference's wait words to a still-working record.
// A terminal record is returned unchanged: its output is the whole answer.
func ApplyWaitHint(
	record TaskOutput,
	hint WaitHint,
	requested, waited time.Duration,
) TaskOutput {
	if !record.Live() {
		return record
	}

	record.Output += "\n\n" + waitHintText(hint, record.Subject, requested, waited)

	return record
}

// waitHintText renders the reference's still-running hint for one subject.
func waitHintText(hint WaitHint, subject Subject, requested, waited time.Duration) string {
	tail := stillRunningDoNotCancel(subject) + " just because this wait returned. " +
		"It is still working. You will be notified automatically when it " +
		"completes. Do other work, or wait again with a longer timeout_ms."

	switch hint {
	case WaitHintReturnedEarly:
		return "Wait returned early because another finished. " + tail
	case WaitHintElapsed:
		waitedLabel := formatWaitedDuration(waited)
		if requested > waited {
			return "Waited " + waitedLabel + ", the per-call maximum, of the " +
				formatWaitedDuration(requested) + " you requested. " + tail
		}

		return "Waited the requested " + waitedLabel + ". " + tail
	default:
		return "Use timeout_ms to wait for completion. " + tail
	}
}

// stillRunningDoNotCancel is the reference's do-not-cancel clause. A background
// task has no follow-up channel, so only a child is told not to be stopped.
func stillRunningDoNotCancel(subject Subject) string {
	if subject == SubjectSubagent {
		return "Unless the user specified, do not kill this subagent and do not tell it to stop"
	}

	return "Unless the user specified, do not kill this task"
}

// formatWaitedDuration renders one waited duration the way the reference's
// hint does: whole seconds above a second, milliseconds below.
func formatWaitedDuration(duration time.Duration) string {
	if duration < time.Second {
		return strconv.FormatInt(duration.Milliseconds(), 10) + "ms"
	}

	return strconv.FormatInt(int64(duration/time.Second), 10) + "s"
}

// formatDurationSeconds renders one duration in hundredths of a second, the
// precision the reference's record body uses.
func formatDurationSeconds(seconds float64) string {
	return strconv.FormatFloat(seconds, 'f', 2, 64) + "s"
}
