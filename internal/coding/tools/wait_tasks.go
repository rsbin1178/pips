package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/jsonx"
)

const waitTasksSchema = `{
 "type":"object","additionalProperties":false,"required":["task_ids","mode"],
 "properties":{
  "task_ids":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"string","minLength":1},"description":"The task ids a monitor Tool call returned."},
  "mode":{"type":"string","enum":["wait_any","wait_all"],"description":"wait_all waits until every id completes; wait_any returns at the first completion."},
  "timeout_ms":{"type":"integer","minimum":0,"maximum":3600000,"description":"Max wait time in milliseconds, up to 3600000 (1 hour). Omit or pass 0 for the 30000ms default: this Tool always waits."}
 }
}`

// NewWaitTasksTool builds the model-facing blocking-wait Tool. It blocks until
// the listed tasks complete, or until the budget elapses, and reports the same
// record the task-output Tool does. It exists because its mode carries the one
// behaviour the task-output Tool has no place for: wait_any returns at the
// first completion.
func NewWaitTasksTool(read ReadBackgroundTasks, known KnownBackgroundIDs) agent.Tool {
	return &waitTasksTool{read: read, known: known}
}

type waitTasksTool struct {
	read  ReadBackgroundTasks
	known KnownBackgroundIDs
}

func (*waitTasksTool) Decl() ai.Tool {
	return ai.Tool{
		Name: WaitTasksName,
		Description: "Wait for background tasks started by the monitor tool to complete. " +
			"Pass task_ids and a mode: wait_all waits until every id completes, " +
			"wait_any returns at the first completion. " +
			"Prefer get_task_output with task_ids and a positive timeout_ms; this Tool is the any-mode alias. " +
			"A wait is always intended here, so an omitted or zero timeout_ms waits 30000ms; " +
			"the ceiling is 3600000. The result is the same record get_task_output reports. " +
			"A not_found result lists the ids the registry still knows.",
		InputSchema: &ai.Schema{RawJSON: json.RawMessage(waitTasksSchema)},
	}
}

type waitTasksArgs struct {
	TaskIDs   []string `json:"task_ids"`
	Mode      string   `json:"mode"`
	TimeoutMS *int64   `json:"timeout_ms"`
}

func (t *waitTasksTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	if call.Name != WaitTasksName {
		return nil, failure(WaitTasksName, fmt.Errorf("%w: mismatched tool name", errInvalidArgument))
	}

	var args waitTasksArgs

	if err := jsonx.Decode(call.Args, &args); err != nil {
		return nil, failure(WaitTasksName, fmt.Errorf(
			"%w: arguments must be one object with task_ids, mode, and an optional timeout_ms",
			errInvalidArgument,
		))
	}

	if args.Mode != TaskOutputModeWaitAny && args.Mode != TaskOutputModeWaitAll {
		return nil, failure(WaitTasksName, fmt.Errorf(
			"%w: mode must be wait_any or wait_all", errInvalidArgument,
		))
	}

	ids, err := resolveTaskOutputIDs(args.TaskIDs, "")
	if err != nil {
		return nil, failure(WaitTasksName, err)
	}

	if t.read == nil {
		return nil, failure(WaitTasksName, errors.New("background task read is unavailable"))
	}

	// A wait is the point of this Tool, so an omitted or zero timeout_ms is the
	// default budget rather than a snapshot.
	requested, budget := waitTasksBudget(args.TimeoutMS)

	records, outcome := t.read(ctx, ids, args.Mode, budget)
	records = applyTaskOutputWaitHint(records, true, outcome, requested, budget)

	value := result{OK: true, Tool: WaitTasksName}

	if !slices.ContainsFunc(records, func(record background.TaskOutput) bool { return record.Found }) {
		value = taskOutputNotFound(WaitTasksName, ids[0], boundedKnownIDs(ctx, t.known))
	} else {
		summary := taskOutputSummary(records, args.Mode)
		value.TaskOutputs = &ResultTaskOutputs{
			Mode: args.Mode, Results: taskOutputPayloads(records), Summary: summary,
		}
		value.Body = background.RenderMultiTaskOutputBody(args.Mode, records, summary)
	}

	value.Truncated = taskOutputTruncated(records)

	return agent.TextResult(value.render()), nil
}

// waitTasksBudget resolves an optional timeout into the requested and effective
// budgets. A wait is always intended on this Tool, so an omitted or zero value
// resolves to the default budget rather than a snapshot, and the effective
// budget is clamped to the one-hour ceiling.
func waitTasksBudget(timeoutMS *int64) (time.Duration, time.Duration) {
	requested := time.Duration(defaultWaitMS) * time.Millisecond
	if timeoutMS != nil && *timeoutMS > 0 {
		requested = time.Duration(*timeoutMS) * time.Millisecond
	}

	return requested, min(requested, maxWaitBlock)
}
