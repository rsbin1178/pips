package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateGoalExec runs one call through the real Tool and returns the summary the
// model would read.
func updateGoalExec(t *testing.T, update tools.UpdateGoal, args string) (string, error) {
	t.Helper()

	parts, err := tools.NewUpdateGoalTool(update).Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: tools.UpdateGoalName, Args: ai.JSON(args),
	})
	if err != nil {
		return "", err
	}

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	var body struct {
		Success bool   `json:"success"`
		Summary string `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(text.Text), &body))
	require.True(t, body.Success)

	return body.Summary, nil
}

// TestUpdateGoalDeclaresTheReferencesThreeOptionalFields pins the declaration the
// model reads: the three channels, none of them required.
func TestUpdateGoalDeclaresTheReferencesThreeOptionalFields(t *testing.T) {
	t.Parallel()

	decl := tools.NewUpdateGoalTool(func(context.Context, tools.GoalDeclaration) (string, error) {
		return "", nil
	}).Decl()
	assert.Equal(t, tools.UpdateGoalName, decl.Name)
	assert.Contains(t, decl.Description, "blocked_reason")
	assert.Contains(t, decl.Description, "independent evidence audit")

	encoded, err := json.Marshal(decl.InputSchema)
	require.NoError(t, err)

	var schema struct {
		Type                 string                     `json:"type"`
		Required             []string                   `json:"required"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(encoded, &schema))
	assert.Equal(t, "object", schema.Type)
	assert.Empty(t, schema.Required)
	assert.ElementsMatch(t, []string{"completed", "message", "blocked_reason"},
		keysOf(schema.Properties))
	require.NotNil(t, schema.AdditionalProperties)
	assert.False(t, *schema.AdditionalProperties)
	assert.Contains(t, string(schema.Properties["blocked_reason"]), "FAILURE signal")
	assert.Contains(t, string(schema.Properties["completed"]), "ONLY when the goal is fully achieved")
}

func keysOf[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	return keys
}

// TestUpdateGoalRoutesEachFieldToTheChannel proves the wrapper hands the runtime
// the model's own declaration and echoes its answer.
func TestUpdateGoalRoutesEachFieldToTheChannel(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		args     string
		want     tools.GoalDeclaration
		answered string
	}{
		{
			name: "progress note", args: `{"message":"halfway there"}`,
			want:     tools.GoalDeclaration{Message: "halfway there"},
			answered: "halfway there",
		},
		{
			name: "completion claim", args: `{"completed":true,"message":"all checks pass"}`,
			want:     tools.GoalDeclaration{Completed: true, Message: "all checks pass"},
			answered: "Goal completion declared",
		},
		{
			name: "failure signal", args: `{"blocked_reason":"three attempts failed"}`,
			want:     tools.GoalDeclaration{BlockedReason: "three attempts failed"},
			answered: "Goal paused as blocked",
		},
		{
			name: "empty update", args: `{}`,
			want:     tools.GoalDeclaration{},
			answered: "Goal updated.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var got tools.GoalDeclaration

			summary, err := updateGoalExec(t, func(_ context.Context, declaration tools.GoalDeclaration) (string, error) {
				got = declaration

				return test.answered, nil
			}, test.args)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
			assert.Equal(t, test.answered, summary)
		})
	}
}

// TestUpdateGoalAcceptsTheLenientCompletedSpellings pins the reference's lenient
// option bool: a model that spells the flag as a string or a number is answered,
// not refused.
func TestUpdateGoalAcceptsTheLenientCompletedSpellings(t *testing.T) {
	t.Parallel()

	for _, spelling := range []string{`true`, `"true"`, `1`, `"yes"`} {
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()

			var got tools.GoalDeclaration

			_, err := updateGoalExec(t, func(_ context.Context, declaration tools.GoalDeclaration) (string, error) {
				got = declaration

				return "accepted", nil
			}, `{"completed":`+spelling+`}`)
			require.NoError(t, err)
			assert.True(t, got.Completed)
		})
	}

	var got tools.GoalDeclaration

	_, err := updateGoalExec(t, func(_ context.Context, declaration tools.GoalDeclaration) (string, error) {
		got = declaration

		return "accepted", nil
	}, `{"completed":false}`)
	require.NoError(t, err)
	assert.False(t, got.Completed)
}

// TestUpdateGoalRefusesMalformedArguments covers the wrapper's own input
// validation, which never reaches the runtime channel.
func TestUpdateGoalRefusesMalformedArguments(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		args string
		want string
	}{
		{name: "not an object", args: `"completed"`, want: "must be one object"},
		{name: "unknown field", args: `{"done":true}`, want: "must be one object"},
		{
			name: "oversize reason",
			args: `{"blocked_reason":"` + strings.Repeat("x", 4097) + `"}`,
			want: "blocked_reason exceeds 4096 bytes",
		},
		{name: "wrong shape", args: `{"completed":{"value":true}}`, want: "must be one object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			reached := false
			_, err := updateGoalExec(t, func(context.Context, tools.GoalDeclaration) (string, error) {
				reached = true

				return "", nil
			}, test.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
			assert.False(t, reached, "a malformed call must not reach the runtime channel")
		})
	}
}

// TestUpdateGoalRefusesWithoutAChannel pins the surface a Runtime that owns no
// Goal harness builds: the Tool refuses every call rather than panicking.
func TestUpdateGoalRefusesWithoutAChannel(t *testing.T) {
	t.Parallel()

	_, err := updateGoalExec(t, nil, `{"completed":true}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unavailable")

	catalogValue, err := tools.NewUpdateGoalCatalog(nil)
	require.NoError(t, err)

	snapshot, err := catalogValue.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskPrivileged))
	require.NoError(t, err)
	assert.Empty(t, snapshot)
}

// TestUpdateGoalRendersTheChannelsNamedRefusal pins the named reason a refusal
// carries, which is what a model reads to correct itself.
func TestUpdateGoalRendersTheChannelsNamedRefusal(t *testing.T) {
	t.Parallel()

	_, err := updateGoalExec(t, func(context.Context, tools.GoalDeclaration) (string, error) {
		return "", tools.ErrGoalNotRunning
	}, `{"completed":true}`)
	require.Error(t, err)

	header, body, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.False(t, header.OK)
	assert.Equal(t, tools.ErrGoalNotRunning.ToolErrorCode(), header.Code)
	assert.Equal(t, "goal_not_running", header.Code)
	assert.Equal(t, tools.ErrGoalNotRunning.Error(), body)
	assert.ErrorIs(t, err, tools.ErrGoalNotRunning)
}
