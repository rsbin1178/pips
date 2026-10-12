package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/jsonx"
)

// KillBackgroundName is the model-facing name of the Tool that stops one
// background task. It is the name the monitor Tool's own description and the
// catch-up notice already promise.
const KillBackgroundName = background.KillTaskToolName

const killBackgroundSchema = `{
 "type":"object","additionalProperties":false,"required":["task_id"],
 "properties":{
  "task_id":{"type":"string","minLength":1,"description":"The task_id a monitor Tool call returned."}
 }
}`

// KillBackground is the one operation a stop routes through. The model Tool,
// the tasks-pane control, and the runtime control plane all call the same
// method, so there is never a second control path to keep in step.
type KillBackground func(context.Context, string) (background.Outcome, error)

// NewKillBackgroundTool builds the model-facing stop Tool. It addresses one
// background task id and reports the registry's own outcome vocabulary:
// killed, already_exited, or not_found.
func NewKillBackgroundTool(kill KillBackground) agent.Tool {
	return &killBackgroundTool{kill: kill}
}

type killBackgroundTool struct {
	kill KillBackground
}

func (*killBackgroundTool) Decl() ai.Tool {
	return ai.Tool{
		Name: KillBackgroundName,
		Description: "Stop one background task started by the monitor tool. " +
			"Address it by the task_id the monitor call returned. " +
			"The outcome is killed, already_exited, or not_found.",
		InputSchema: &ai.Schema{RawJSON: json.RawMessage(killBackgroundSchema)},
	}
}

type killBackgroundArgs struct {
	TaskID string `json:"task_id"`
}

func (t *killBackgroundTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	if call.Name != KillBackgroundName {
		return nil, failure(KillBackgroundName, fmt.Errorf("%w: mismatched tool name", errInvalidArgument))
	}

	var args killBackgroundArgs

	if err := jsonx.Decode(call.Args, &args); err != nil {
		return nil, failure(KillBackgroundName, fmt.Errorf("%w: arguments must be one object with task_id", errInvalidArgument))
	}

	if args.TaskID == "" {
		return nil, failure(KillBackgroundName, fmt.Errorf("%w: task_id is required", errInvalidArgument))
	}

	if t.kill == nil {
		return nil, failure(KillBackgroundName, errors.New("background control is unavailable"))
	}

	outcome, err := t.kill(ctx, args.TaskID)
	if err != nil {
		return nil, failure(KillBackgroundName, err)
	}

	value := result{OK: true, Tool: KillBackgroundName, Kill: &ResultKill{
		TaskID: args.TaskID, Outcome: string(outcome),
	}}
	value.Body = killBackgroundBody(args.TaskID, outcome)

	if outcome == background.OutcomeNotFound {
		value.OK = false
		value.Code = string(background.OutcomeNotFound)
	}

	return agent.TextResult(value.render()), nil
}

// killBackgroundBody is the model-facing sentence for one outcome. The task id
// is repeated because the sentence is what a reader quotes back.
func killBackgroundBody(taskID string, outcome background.Outcome) string {
	switch outcome {
	case background.OutcomeKilled:
		return "Task \"" + taskID + "\" was terminated successfully."
	case background.OutcomeAlreadyExited:
		return "Task \"" + taskID + "\" had already completed."
	default:
		return "Task \"" + taskID + "\" not found."
	}
}
