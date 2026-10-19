package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/jsonx"
)

// KillBackgroundName is the model-facing name of the Tool that stops one
// background task. It is the name the monitor Tool's own description and the
// catch-up notice already promise.
const KillBackgroundName = background.KillTaskToolName

const killBackgroundSchema = `{
 "type":"object","additionalProperties":false,"required":["task_id"],
 "properties":{
  "task_id":{"type":"string","minLength":1,"description":"The task_id a shell or monitor Tool call returned, or the subagent_id a spawn Tool call returned."}
 }
}`

// KillBackground is the one operation a stop routes through. The model Tool,
// the tasks-pane control, and the runtime control plane all call the same
// method, so there is never a second control path to keep in step. The subject
// comes back beside the outcome because the two producers are worded
// differently: a background task is stopped outright, while a child's
// cancellation is only initiated and the child winds down afterwards.
type KillBackground func(context.Context, string) (background.Outcome, background.Subject, error)

// KnownBackgroundIDs lists the ids the control surface still knows, so a
// not-found stop can name them instead of leaving the model with no way to
// recover a lost id. It is optional; a nil value omits the enumeration.
type KnownBackgroundIDs func(context.Context) []string

const (
	// maxKillKnownIDs bounds how many known ids one not-found sentence lists,
	// and maxKillKnownIDBytes bounds the whole list's length, ellipsis included.
	maxKillKnownIDs      = 20
	maxKillKnownIDBytes  = 512
	killKnownIDsEllipsis = ", …"
)

// NewKillBackgroundTool builds the model-facing stop Tool. It addresses one
// background task or child id and reports the registry's own outcome
// vocabulary: killed, already_exited, or not_found. A not-found stop enumerates
// the ids the registry knows, so a model that lost an id can recover.
func NewKillBackgroundTool(kill KillBackground, known KnownBackgroundIDs) agent.Tool {
	return &killBackgroundTool{kill: kill, known: known}
}

type killBackgroundTool struct {
	kill  KillBackground
	known KnownBackgroundIDs
}

func (*killBackgroundTool) Decl() ai.Tool {
	return ai.Tool{
		Name: KillBackgroundName,
		Description: "Stop one background task or subagent by the id its start returned: " +
			"a backgrounded command or a watch from the shell or monitor tool, or a spawned " +
			"child from the " + subagent.ToolName + " tool. " +
			"The outcome is killed, already_exited, or not_found; " +
			"a not_found result lists the ids the registry still knows.",
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

	outcome, subject, err := t.kill(ctx, args.TaskID)
	if err != nil {
		return nil, failure(KillBackgroundName, err)
	}

	value := result{OK: true, Tool: KillBackgroundName, Kill: &ResultKill{
		TaskID: args.TaskID, Outcome: string(outcome),
	}}
	value.Body = killBackgroundBody(args.TaskID, outcome, subject, boundedKnownIDs(ctx, t.known))

	if outcome == background.OutcomeNotFound {
		value.OK = false
		value.Code = string(background.OutcomeNotFound)
	}

	return agent.TextResult(value.render()), nil
}

// boundedKnownIDs reads the control surface's known ids, bounded in count. A
// nil or failing read yields nothing rather than failing the call, so every
// not-found sentence shares one enumeration.
func boundedKnownIDs(ctx context.Context, known KnownBackgroundIDs) []string {
	if known == nil {
		return nil
	}

	ids := known(ctx)
	if len(ids) > maxKillKnownIDs {
		ids = ids[:maxKillKnownIDs]
	}

	return ids
}

// killBackgroundBody is the model-facing sentence for one outcome, worded for
// the subject the stop resolved. The task id is repeated because the sentence
// is what a reader quotes back, and a not-found sentence names the ids the
// registry knows so a lost id is recoverable.
//
// The two producers get different sentences on purpose. A background task is
// stopped outright, so the call knows it was terminated. A child's cancellation
// is only initiated — the child winds down afterwards — so claiming it was
// terminated successfully would say more than the call knows.
func killBackgroundBody(
	taskID string,
	outcome background.Outcome,
	subject background.Subject,
	known []string,
) string {
	if subject == background.SubjectSubagent {
		switch outcome {
		case background.OutcomeKilled:
			return "Subagent \"" + taskID + "\" cancellation was initiated."
		case background.OutcomeAlreadyExited:
			return "Subagent \"" + taskID + "\" had already finished."
		default:
			return notFoundSentence(taskID, known)
		}
	}

	switch outcome {
	case background.OutcomeKilled:
		return "Task \"" + taskID + "\" was terminated successfully."
	case background.OutcomeAlreadyExited:
		return "Task \"" + taskID + "\" had already completed."
	default:
		return notFoundSentence(taskID, known)
	}
}

// notFoundSentence is the one not-found sentence a background-task Tool
// reports. It names both kinds the stop resolves, because one id addresses a
// task and a child alike, and it names the ids the registry still knows, bounded
// in count and length, so a model that lost an id can recover it. The list stays
// the registry's own: the reference's own enumeration is labelled "known bash
// task IDs" and lists no subagent either, and it is the enumeration the one
// not-found sentence is built from.
func notFoundSentence(taskID string, known []string) string {
	body := "Task or subagent \"" + taskID + "\" not found."
	if listed := knownIDsText(known); listed != "" {
		body += " Known task ids: " + listed + "."
	}

	return body
}

// knownIDsText renders the bounded, comma-separated list of known ids.
func knownIDsText(known []string) string {
	listed := ""

	for _, id := range known {
		if id == "" {
			continue
		}

		next := id
		if listed != "" {
			next = listed + ", " + id
		}

		if len(next)+len(killKnownIDsEllipsis) > maxKillKnownIDBytes {
			if listed == "" {
				return ""
			}

			return listed + killKnownIDsEllipsis
		}

		listed = next
	}

	return listed
}
