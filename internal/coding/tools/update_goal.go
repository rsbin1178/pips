package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/jsonx"
)

// UpdateGoalName is the model-facing name of the Tool that declares progress,
// blockage, or completion for the running Goal.
const UpdateGoalName = "update_goal"

// maxGoalDeclarationBytes bounds one text channel at the Goal pause reason's own
// bound, so an over-long declaration is refused as an argument instead of
// failing the pause it asked for.
const maxGoalDeclarationBytes = 4096

// GoalDeclaration is one model declaration about the running Goal: a progress
// note, a failure signal, or a claim of completion. The three channels are the
// reference's own, and a declaration is never evidence — the auditor reads the
// workspace and the recorded tool results, not the worker's confidence.
type GoalDeclaration struct {
	// Completed reports that the Goal is fully achieved. It moves when the
	// completion question is asked, never who answers it.
	Completed bool
	// Message is a short progress note, recorded and echoed without changing the
	// run.
	Message string
	// BlockedReason is a failure signal: the same problem has defeated three or
	// more attempts. It pauses the Goal for a user decision.
	BlockedReason string
}

// Validate bounds the two text channels.
func (d GoalDeclaration) Validate() error {
	if err := validateGoalText("message", d.Message); err != nil {
		return err
	}

	return validateGoalText("blocked_reason", d.BlockedReason)
}

func validateGoalText(field, value string) error {
	if len(value) > maxGoalDeclarationBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", errInvalidArgument, field, maxGoalDeclarationBytes)
	}

	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", errInvalidArgument, field)
	}

	return nil
}

// UpdateGoal is the one runtime channel a declaration routes through. It
// answers with the sentence the model reads, or refuses with a named reason. A
// Tool built without one refuses every call, which is what a surface that owns
// no Goal harness still wants.
type UpdateGoal func(context.Context, GoalDeclaration) (string, error)

// GoalError names why a declaration was refused. The code is the stable reason;
// the text is the sentence the model reads.
type GoalError struct {
	Code string
	Text string
}

func (e *GoalError) Error() string { return e.Text }

// ToolErrorCode implements the coded tool failure the result envelope renders.
func (e *GoalError) ToolErrorCode() string { return e.Code }

// Is supports errors.Is against the package sentinels.
func (e *GoalError) Is(target error) bool {
	other, ok := target.(*GoalError)

	return ok && e.Code == other.Code
}

// Refusals the runtime answers a declaration with.
var (
	// ErrGoalNotRunning refuses every declaration when no Goal run exists. The
	// Tool is offered outside a Goal run, so this is a sentence rather than an
	// unknown Tool.
	ErrGoalNotRunning = &GoalError{Code: "goal_not_running", Text: "no Goal run is in progress"}
	// ErrGoalNotActive refuses a completion or failure declaration against a
	// Goal that is paused or already finished.
	ErrGoalNotActive = &GoalError{Code: "goal_not_active", Text: "the Goal is not active"}
)

// NewUpdateGoalCatalog returns the model-facing Goal declaration Tool. It is
// offered wherever the Goal harness can run, and a nil channel offers none.
func NewUpdateGoalCatalog(update UpdateGoal) (*catalog.Catalog, error) {
	if update == nil {
		return catalog.New()
	}

	return catalog.New(localEntry(NewUpdateGoalTool(update), catalog.RiskRead, "planning", "goal"))
}

// updateGoalArgs is the model-facing input. Every field is optional, and the
// three descriptions carry the reference's own wording.
type updateGoalArgs struct {
	Completed     *lenientGoalBool `json:"completed,omitempty" description:"Set to true ONLY when the goal is fully achieved. This ends goal mode. Use together with message to include a completion summary."`
	Message       string           `json:"message,omitempty" description:"Optional short message logged as progress (visible in the tool response). Use with completed: true for a completion summary."`
	BlockedReason string           `json:"blocked_reason,omitempty" description:"Set only when truly stuck after 3+ consecutive failed attempts at the same problem. If set, the goal is paused as blocked. This is a FAILURE signal — never put success text here. For success, use completed: true with message."`
}

// lenientGoalBool accepts the JSON booleans plus the string and number spellings
// a model may emit for completed, as the reference's own lenient option bool
// does.
type lenientGoalBool bool

func (b *lenientGoalBool) UnmarshalJSON(data []byte) error {
	value, err := decodeLenientBool(json.RawMessage(data))
	if err != nil {
		return err
	}

	*b = lenientGoalBool(value)

	return nil
}

// NewUpdateGoalTool builds the model-facing declaration Tool. It carries the one
// runtime channel that owns the Goal, so the declaration is answered against the
// run's own state.
func NewUpdateGoalTool(update UpdateGoal) agent.Tool {
	schema, err := ai.SchemaFor[updateGoalArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding tools: update_goal schema: %v", err))
	}
	// Every field is optional: the model may log a note, declare completion,
	// flag a block, or send an empty update.
	schema.Required = nil

	return &updateGoalTool{update: update, decl: ai.Tool{
		Name: UpdateGoalName,
		Description: "Report progress on the active Goal: log a short status message, " +
			"declare the Goal completed, or flag that the same problem has defeated three or " +
			"more attempts. A completion declaration does not decide completion: an " +
			"independent evidence audit reads the workspace and the recorded tool results at " +
			"the Goal loop's next decision point, and its verdict is final. A blocked_reason " +
			"pauses the Goal for a user decision.",
		InputSchema: schema,
	}}
}

type updateGoalTool struct {
	update UpdateGoal
	decl   ai.Tool
}

func (t *updateGoalTool) Decl() ai.Tool { return t.decl }

func (t *updateGoalTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	if call.Name != UpdateGoalName {
		return nil, failure(UpdateGoalName, fmt.Errorf("%w: mismatched tool name", errInvalidArgument))
	}

	var args updateGoalArgs

	if err := jsonx.Decode(call.Args, &args); err != nil {
		return nil, failure(UpdateGoalName, fmt.Errorf(
			"%w: arguments must be one object with completed, message, and/or blocked_reason",
			errInvalidArgument,
		))
	}

	declaration := GoalDeclaration{
		Completed:     args.Completed != nil && bool(*args.Completed),
		Message:       strings.TrimSpace(args.Message),
		BlockedReason: strings.TrimSpace(args.BlockedReason),
	}
	if err := declaration.Validate(); err != nil {
		return nil, failure(UpdateGoalName, err)
	}

	if t.update == nil {
		return nil, failure(UpdateGoalName, errors.New("goal declarations are unavailable"))
	}

	summary, err := t.update(ctx, declaration)
	if err != nil {
		// The channel's error is its own named refusal, which the model must read
		// as the sentence it is.
		return nil, failure(UpdateGoalName, err)
	}

	data, err := json.Marshal(struct {
		Success bool   `json:"success"`
		Summary string `json:"summary"`
	}{Success: true, Summary: summary})
	if err != nil {
		return nil, failure(UpdateGoalName, fmt.Errorf("coding tools: encode update_goal result: %w", err))
	}

	return agent.TextResult(string(data)), nil
}
