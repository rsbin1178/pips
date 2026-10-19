package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/jsonx"
)

// UseToolName is the model-facing name of the Tool that calls a tool the current
// snapshot hides, by name.
const UseToolName = "use_tool"

const useToolSchema = `{
 "type":"object","additionalProperties":false,"required":["tool_name","tool_input"],
 "properties":{
  "tool_name":{"type":"string","minLength":1,"description":"Exact name of the tool to call, as the tool search reported it."},
  "tool_input":{"type":"object","additionalProperties":true,"description":"The target tool's own arguments object, matching the schema the tool search returned."}
 }
}`

// NewUseToolTool builds the model-facing dispatch Tool. It carries the one
// dispatcher that owns the catalog and the policy, so a name it reaches is
// resolved exactly as a direct call would be resolved, and the target's own
// result is what the model receives. A Tool built without a dispatcher refuses
// every call, which is what a surface that cannot dispatch still wants.
func NewUseToolTool(dispatch catalog.UseToolDispatch) agent.Tool {
	return &useToolTool{dispatch: dispatch}
}

type useToolTool struct {
	dispatch catalog.UseToolDispatch
}

func (*useToolTool) Decl() ai.Tool {
	return ai.Tool{
		Name: UseToolName,
		Description: "Call a tool discovered through the tool search surface by its exact name, " +
			"passing that tool's own arguments as tool_input. " +
			"Only a tool the current snapshot hides can be reached this way: " +
			"a tool that is already visible must be called as its own tool call.",
		InputSchema: &ai.Schema{RawJSON: json.RawMessage(useToolSchema)},
	}
}

type useToolArgs struct {
	ToolName  string  `json:"tool_name"`
	ToolInput ai.JSON `json:"tool_input"`
}

func (t *useToolTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	if call.Name != UseToolName {
		return nil, failure(UseToolName, fmt.Errorf("%w: mismatched tool name", errInvalidArgument))
	}

	var args useToolArgs

	if err := jsonx.Decode(call.Args, &args); err != nil {
		return nil, failure(UseToolName, fmt.Errorf(
			"%w: arguments must be one object with tool_name and tool_input",
			errInvalidArgument,
		))
	}

	name := strings.TrimSpace(args.ToolName)
	if name == "" {
		return nil, failure(UseToolName, fmt.Errorf("%w: tool_name is required", errInvalidArgument))
	}

	input, err := useToolInput(args.ToolInput)
	if err != nil {
		return nil, failure(UseToolName, err)
	}

	if t.dispatch == nil {
		return nil, failure(UseToolName, errors.New("tool dispatch is unavailable"))
	}

	parts, err := t.dispatch(ctx, name, input)
	if err != nil {
		// The dispatcher's error is either its own refusal, which the model must
		// read as the sentence it is, or the target's own failure envelope, which
		// the wrapper must not restate.
		return nil, err
	}

	return parts, nil
}

// useToolInput validates the target's own arguments. The target's schema is the
// only authority on its fields, so the wrapper insists on an object — what every
// pips Tool takes — and passes the model's value on unchanged.
func useToolInput(raw ai.JSON) (ai.JSON, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("%w: tool_input is required", errInvalidArgument)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%w: tool_input must be a JSON object", errInvalidArgument)
	}

	return raw, nil
}
