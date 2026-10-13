package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/jsonx"
)

const (
	// TaskOutputName is the model-facing name of the Tool that reads one
	// background task's output by id. It is the name a completion's pointer
	// line promises.
	TaskOutputName = background.TaskOutputToolName
	// WaitTasksName is the model-facing name of the Tool that blocks until
	// background tasks complete.
	WaitTasksName = "wait_tasks"

	// TaskOutputModePoll is a non-blocking snapshot.
	TaskOutputModePoll = "poll"
	// TaskOutputModeWaitAll waits for every asked id.
	TaskOutputModeWaitAll = "wait_all"
	// TaskOutputModeWaitAny returns at the first completion.
	TaskOutputModeWaitAny = "wait_any"

	// maxTaskOutputIDs is the reference's multi-id cap.
	maxTaskOutputIDs = 20
	// maxWaitBlockMS is the fixed blocking-wait ceiling: one hour. pips takes
	// the reference's value without its environment override.
	maxWaitBlockMS = 3_600_000
	// defaultWaitMS is the budget a wait uses when the model asks to wait
	// without naming a duration.
	defaultWaitMS = 30_000
)

// maxWaitBlock is the fixed blocking-wait ceiling as a duration.
const maxWaitBlock = maxWaitBlockMS * time.Millisecond

// ReadBackgroundTasks resolves background tasks by id. mode is one of the
// TaskOutputMode constants; budget is the effective wait budget, zero for a
// poll. Records come back ordered as the ids were asked, with an unknown id
// reported as an unfound record.
type ReadBackgroundTasks func(
	ctx context.Context,
	ids []string,
	mode string,
	budget time.Duration,
) ([]background.TaskOutput, background.WaitOutcome)

const taskOutputSchema = `{
 "type":"object","additionalProperties":false,
 "properties":{
  "task_ids":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"string","minLength":1},"description":"The task ids a shell or monitor Tool call returned. Pass one or more; for a single task use a one-element array. Multiple ids with a positive timeout_ms wait until all complete."},
  "task_id":{"type":"string","minLength":1,"description":"Alias for a single task id: task_ids with one element."},
  "timeout_ms":{"type":"integer","minimum":0,"maximum":3600000,"description":"Max wait time in milliseconds, up to 3600000 (1 hour). A positive value waits for completion; omit or pass 0 for a non-blocking status snapshot."}
 }
}`

// NewTaskOutputTool builds the model-facing task-output Tool. It reads one or
// more background tasks by id and reports each one's status, output, and exit
// code; a positive timeout_ms waits instead of taking a snapshot. A not-found
// id enumerates the ids the registry knows, so a model that lost an id can
// recover it.
func NewTaskOutputTool(read ReadBackgroundTasks, known KnownBackgroundIDs) agent.Tool {
	return &taskOutputTool{read: read, known: known}
}

type taskOutputTool struct {
	read  ReadBackgroundTasks
	known KnownBackgroundIDs
}

func (*taskOutputTool) Decl() ai.Tool {
	return ai.Tool{
		Name: TaskOutputName,
		Description: "Get output and status from a background task started by the shell or monitor tool. " +
			"Pass task_ids with one or more ids from a shell or monitor call, or the single-id alias task_id. " +
			"Multiple ids with a positive timeout_ms wait until all complete. " +
			"Omit timeout_ms or pass 0 for a non-blocking status snapshot; " +
			"a positive timeout_ms waits up to that many milliseconds, capped at 3600000. " +
			"Returns the current output, status, and exit code if completed. " +
			"If the output is large, use read on the output_file path for the rest. " +
			"A not_found result lists the ids the registry still knows.",
		InputSchema: &ai.Schema{RawJSON: json.RawMessage(taskOutputSchema)},
	}
}

type taskOutputArgs struct {
	TaskIDs   []string `json:"task_ids"`
	TaskID    string   `json:"task_id"`
	TimeoutMS *int64   `json:"timeout_ms"`
}

func (t *taskOutputTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	if call.Name != TaskOutputName {
		return nil, failure(TaskOutputName, fmt.Errorf("%w: mismatched tool name", errInvalidArgument))
	}

	var args taskOutputArgs

	if err := jsonx.Decode(call.Args, &args); err != nil {
		return nil, failure(TaskOutputName, fmt.Errorf(
			"%w: arguments must be one object with task_ids or task_id and an optional timeout_ms",
			errInvalidArgument,
		))
	}

	ids, err := resolveTaskOutputIDs(args.TaskIDs, args.TaskID)
	if err != nil {
		return nil, failure(TaskOutputName, err)
	}

	if t.read == nil {
		return nil, failure(TaskOutputName, errors.New("background task read is unavailable"))
	}

	waiting, requested, budget := taskOutputWait(args.TimeoutMS)
	mode := TaskOutputModePoll

	if waiting {
		mode = TaskOutputModeWaitAll
	}

	records, outcome := t.read(ctx, ids, mode, budget)
	records = applyTaskOutputWaitHint(records, waiting, outcome, requested, budget)

	value := t.renderTaskOutputs(ctx, ids, records, mode)
	value.Truncated = taskOutputTruncated(records)

	return agent.TextResult(value.render()), nil
}

// taskOutputWait resolves an optional timeout into the wait flag and the
// requested and effective budgets. The effective budget is clamped to the
// one-hour ceiling; an omitted or zero timeout is a snapshot, not a wait.
func taskOutputWait(timeoutMS *int64) (bool, time.Duration, time.Duration) {
	if timeoutMS == nil || *timeoutMS <= 0 {
		return false, 0, 0
	}

	requested := time.Duration(*timeoutMS) * time.Millisecond

	return true, requested, min(requested, maxWaitBlock)
}

// renderTaskOutputs projects the records into the tool result. One id renders
// the single shape; several render the reference's multi shape. A list where
// nothing is known is a failed call, so the model does not read it as a read.
func (t *taskOutputTool) renderTaskOutputs(
	ctx context.Context,
	ids []string,
	records []background.TaskOutput,
	mode string,
) result {
	if len(ids) == 1 {
		record := records[0]
		if !record.Found {
			return taskOutputNotFound(TaskOutputName, record.TaskID, boundedKnownIDs(ctx, t.known))
		}

		payload := taskOutputPayload(record)

		return result{
			OK: true, Tool: TaskOutputName, TaskOutput: &payload,
			Body: background.RenderTaskOutputBody(record),
		}
	}

	if !slices.ContainsFunc(records, func(record background.TaskOutput) bool { return record.Found }) {
		return taskOutputNotFound(TaskOutputName, ids[0], boundedKnownIDs(ctx, t.known))
	}

	summary := taskOutputSummary(records, mode)

	return result{
		OK: true, Tool: TaskOutputName,
		TaskOutputs: &ResultTaskOutputs{
			Mode: mode, Results: taskOutputPayloads(records), Summary: summary,
		},
		Body: background.RenderMultiTaskOutputBody(mode, records, summary),
	}
}

// taskOutputNotFound is the one not-found result both task-output Tools report:
// the stop Tool's shape, a not_found code, and the sentence that names the ids
// the registry still knows.
func taskOutputNotFound(tool, taskID string, known []string) result {
	return result{
		OK: false, Tool: tool, Code: string(background.OutcomeNotFound),
		TaskOutput: &ResultTaskOutput{TaskID: taskID, Status: background.StatusNotFound},
		Body:       notFoundSentence(taskID, known),
	}
}

// resolveTaskOutputIDs merges the plural and singular arguments into one
// de-duplicated, trimmed id list, preserving first-seen order.
func resolveTaskOutputIDs(plural []string, singular string) ([]string, error) {
	values := plural
	if len(values) == 0 && strings.TrimSpace(singular) != "" {
		values = []string{singular}
	}

	resolved := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || slices.Contains(resolved, value) {
			continue
		}

		resolved = append(resolved, value)
	}

	if len(resolved) == 0 {
		return nil, fmt.Errorf("%w: task_ids must contain at least one task id", errInvalidArgument)
	}

	if len(resolved) > maxTaskOutputIDs {
		return nil, fmt.Errorf(
			"%w: task_ids exceeds the maximum of %d ids", errInvalidArgument, maxTaskOutputIDs,
		)
	}

	return resolved, nil
}

// applyTaskOutputWaitHint appends the reference's wait words to each
// still-running record.
func applyTaskOutputWaitHint(
	records []background.TaskOutput,
	waiting bool,
	outcome background.WaitOutcome,
	requested, budget time.Duration,
) []background.TaskOutput {
	hint := background.WaitHintPoll
	if waiting {
		hint = background.WaitHintElapsed
		if outcome == background.WaitOutcomeCompleted {
			hint = background.WaitHintReturnedEarly
		}
	}

	applied := make([]background.TaskOutput, 0, len(records))
	for _, record := range records {
		applied = append(applied, background.ApplyWaitHint(record, hint, requested, budget))
	}

	return applied
}

// taskOutputSummary counts the records that reached a terminal status.
func taskOutputSummary(records []background.TaskOutput, mode string) string {
	completed := 0

	for _, record := range records {
		switch record.Status {
		case background.StatusCompleted, background.StatusFailed,
			background.StatusCancelled, background.StatusTimedOut:
			completed++
		}
	}

	return strconv.Itoa(completed) + "/" + strconv.Itoa(len(records)) +
		" tasks completed (" + mode + ")"
}

// taskOutputTruncated reports whether any record's output was cut.
func taskOutputTruncated(records []background.TaskOutput) bool {
	return slices.ContainsFunc(records, func(record background.TaskOutput) bool {
		return record.Truncated
	})
}

func taskOutputPayloads(records []background.TaskOutput) []ResultTaskOutput {
	payloads := make([]ResultTaskOutput, 0, len(records))
	for _, record := range records {
		payloads = append(payloads, taskOutputPayload(record))
	}

	return payloads
}

// taskOutputPayload projects one record into the structured result payload. The
// times are RFC 3339 in UTC, and `ended` stays empty while the task runs.
func taskOutputPayload(record background.TaskOutput) ResultTaskOutput {
	payload := ResultTaskOutput{
		TaskID:         record.TaskID,
		Command:        record.Command,
		Status:         record.Status,
		ExitCode:       record.ExitCode,
		DurationSecs:   record.DurationSecs,
		Output:         record.Output,
		OutputFile:     record.OutputFile,
		Truncated:      record.Truncated,
		TruncationHint: record.TruncationHint,
		RawOutputBytes: record.RawOutputBytes,
	}

	if !record.Started.IsZero() {
		payload.Started = record.Started.UTC().Format(time.RFC3339)
	}

	if !record.Ended.IsZero() {
		payload.Ended = record.Ended.UTC().Format(time.RFC3339)
	}

	return payload
}
