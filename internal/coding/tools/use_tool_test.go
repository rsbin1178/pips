package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useToolFailure runs one call through the real Tool and returns the decoded
// header and body of the failure envelope it answered with.
func useToolFailure(t *testing.T, args string, dispatch catalog.UseToolDispatch) (ResultHeader, string) {
	t.Helper()

	_, err := NewUseToolTool(dispatch).Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: UseToolName, Args: ai.JSON(args),
	})
	require.Error(t, err)

	header, body, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)

	return header, body
}

// TestUseToolDeclaresTheDispatchSurface pins the declaration the model reads: the
// two wrapper arguments, both required, and no place for the file forms the
// reference's other input shapes would add.
func TestUseToolDeclaresTheDispatchSurface(t *testing.T) {
	t.Parallel()

	decl := NewUseToolTool(func(context.Context, string, ai.JSON) ([]ai.Part, error) { return nil, nil }).Decl()
	assert.Equal(t, UseToolName, decl.Name)
	assert.Contains(t, decl.Description, "tool search")
	assert.Contains(t, decl.Description, "called as its own tool call")

	var schema struct {
		Type                 string                     `json:"type"`
		Required             []string                   `json:"required"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(decl.InputSchema.RawJSON, &schema))
	assert.Equal(t, "object", schema.Type)
	assert.ElementsMatch(t, []string{"tool_name", "tool_input"}, schema.Required)
	assert.Len(t, schema.Properties, 2)
	require.NotNil(t, schema.AdditionalProperties)
	assert.False(t, *schema.AdditionalProperties)
}

// TestUseToolRefusesMalformedArgumentsBeforeTheTargetRuns covers every way the
// wrapper's own input can be wrong. None of them may reach the dispatcher, which
// is the only path to a target.
func TestUseToolRefusesMalformedArgumentsBeforeTheTargetRuns(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		args string
		want string
	}{
		{name: "not an object", args: `"read"`, want: "must be one object"},
		{name: "no arguments", args: `{}`, want: "tool_name is required"},
		{name: "blank name", args: `{"tool_name":"  ","tool_input":{}}`, want: "tool_name is required"},
		{name: "no input", args: `{"tool_name":"read"}`, want: "tool_input is required"},
		{name: "null input", args: `{"tool_name":"read","tool_input":null}`, want: "tool_input is required"},
		{name: "scalar input", args: `{"tool_name":"read","tool_input":"{}"}`, want: "tool_input must be a JSON object"},
		{name: "list input", args: `{"tool_name":"read","tool_input":[]}`, want: "tool_input must be a JSON object"},
		{
			// The reference's file forms are deliberately not adopted, so an
			// argument set carrying one is an unknown field, not a second form.
			name: "file form", args: `{"tool_name":"read","tool_input":{},"tool_input_file":"/tmp/mcp.json"}`,
			want: "must be one object",
		},
		{name: "duplicate name", args: `{"tool_name":"read","tool_name":"ls","tool_input":{}}`, want: "must be one object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ran := false
			header, body := useToolFailure(t, test.args, func(context.Context, string, ai.JSON) ([]ai.Part, error) {
				ran = true

				return nil, nil
			})
			assert.False(t, header.OK)
			assert.Equal(t, "invalid_argument", header.Code)
			assert.Contains(t, body, test.want)
			assert.False(t, ran, "the target must not run for a malformed wrapper argument")
		})
	}
}

// TestUseToolReturnsTheTargetResultUnchanged is the wrapper's contract: the
// target's own content is the call's result.
func TestUseToolReturnsTheTargetResultUnchanged(t *testing.T) {
	t.Parallel()

	var (
		gotName  string
		gotInput string
	)

	target := []ai.Part{ai.TextPart{Text: "remote result"}, ai.TextPart{Text: "second part"}}
	tool := NewUseToolTool(func(_ context.Context, name string, input ai.JSON) ([]ai.Part, error) {
		gotName, gotInput = name, string(input)

		return target, nil
	})

	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: UseToolName,
		Args: ai.JSON(`{"tool_name":" docs__lookup ","tool_input":{"query":"parity","limit":3}}`),
	})
	require.NoError(t, err)
	assert.Equal(t, target, parts)
	assert.Equal(t, "docs__lookup", gotName)
	assert.JSONEq(t, `{"query":"parity","limit":3}`, gotInput)
}

// TestUseToolPropagatesTheDispatcherAnswer is the other half: a refusal, an
// unknown name, or the target's own failure arrives as it was produced, so the
// wrapper never restates the sentence the model has to read.
func TestUseToolPropagatesTheDispatcherAnswer(t *testing.T) {
	t.Parallel()

	refusal := errors.New(`tool "read" is directly visible in this session`)
	_, err := NewUseToolTool(func(context.Context, string, ai.JSON) ([]ai.Part, error) {
		return nil, refusal
	}).Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: UseToolName, Args: ai.JSON(`{"tool_name":"read","tool_input":{}}`),
	})
	require.EqualError(t, err, refusal.Error())
}

// TestUseToolRefusesWithoutADispatcher keeps a Tool built by a caller that has
// nothing to dispatch through from panicking or silently succeeding.
func TestUseToolRefusesWithoutADispatcher(t *testing.T) {
	t.Parallel()

	header, body := useToolFailure(t, `{"tool_name":"docs__lookup","tool_input":{}}`, nil)
	assert.False(t, header.OK)
	assert.Equal(t, "io_error", header.Code)
	assert.Contains(t, body, "tool dispatch is unavailable")
}

// TestUseToolRejectsAMismatchedCallName keeps the wrapper's own name check, so a
// call routed to it under another name is an argument error rather than a
// dispatch.
func TestUseToolRejectsAMismatchedCallName(t *testing.T) {
	t.Parallel()

	_, err := NewUseToolTool(func(context.Context, string, ai.JSON) ([]ai.Part, error) {
		return nil, errors.New("must not run")
	}).Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: "other", Args: ai.JSON(`{"tool_name":"read","tool_input":{}}`),
	})
	require.ErrorContains(t, err, "mismatched tool name")
}
