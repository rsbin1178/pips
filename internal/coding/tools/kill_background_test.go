package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// killToolResult runs one stop through the real Tool, with the injected control
// answering the outcome and subject a test asks for.
func killToolResult(
	t *testing.T,
	outcome background.Outcome,
	subject background.Subject,
	err error,
) (ResultHeader, string) {
	t.Helper()

	tool := NewKillBackgroundTool(func(context.Context, string) (background.Outcome, background.Subject, error) {
		return outcome, subject, err
	}, nil)
	require.Equal(t, KillBackgroundName, tool.Decl().Name)
	require.Equal(t, background.KillTaskToolName, tool.Decl().Name)

	parts, execErr := tool.Exec(
		t.Context(),
		agent.ToolCall{
			ID: "call-1", Name: KillBackgroundName,
			Args: ai.JSON(`{"task_id":"monitor-1"}`),
		},
	)
	require.NoError(t, execErr)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	header, body, parseErr := ParseResult(text.Text)
	require.NoError(t, parseErr)

	return header, body
}

// TestKillBackgroundToolReportsTheTaskVocabulary pins a watch's own words: the
// call stopped the task outright, so it reports a termination.
func TestKillBackgroundToolReportsTheTaskVocabulary(t *testing.T) {
	t.Parallel()

	header, body := killToolResult(t, background.OutcomeKilled, background.SubjectTask, nil)
	assert.True(t, header.OK)
	require.NotNil(t, header.Kill)
	assert.Equal(t, "monitor-1", header.Kill.TaskID)
	assert.Equal(t, "killed", header.Kill.Outcome)
	assert.Equal(t, `Task "monitor-1" was terminated successfully.`, body)

	header, body = killToolResult(t, background.OutcomeAlreadyExited, background.SubjectTask, nil)
	assert.True(t, header.OK)
	assert.Equal(t, "already_exited", header.Kill.Outcome)
	assert.Equal(t, `Task "monitor-1" had already completed.`, body)
}

// TestKillBackgroundToolReportsTheChildVocabulary pins a child's own words: a
// cancellation is only initiated, and the child winds down afterwards, so the
// sentence must not claim it was terminated.
func TestKillBackgroundToolReportsTheChildVocabulary(t *testing.T) {
	t.Parallel()

	header, body := killToolResult(t, background.OutcomeKilled, background.SubjectSubagent, nil)
	assert.True(t, header.OK)
	require.NotNil(t, header.Kill)
	assert.Equal(t, "monitor-1", header.Kill.TaskID)
	assert.Equal(t, "killed", header.Kill.Outcome)
	assert.Equal(t, `Subagent "monitor-1" cancellation was initiated.`, body)

	header, body = killToolResult(t, background.OutcomeAlreadyExited, background.SubjectSubagent, nil)
	assert.True(t, header.OK)
	assert.Equal(t, "already_exited", header.Kill.Outcome)
	assert.Equal(t, `Subagent "monitor-1" had already finished.`, body)
}

// TestKillBackgroundToolNamesBothKindsOnNotFound pins the not-found sentence: it
// names both kinds one id can address, and it is a failed call, because the
// model must not read it as a stop.
func TestKillBackgroundToolNamesBothKindsOnNotFound(t *testing.T) {
	t.Parallel()

	for _, subject := range []background.Subject{background.SubjectTask, background.SubjectSubagent} {
		header, body := killToolResult(t, background.OutcomeNotFound, subject, nil)
		assert.False(t, header.OK)
		assert.Equal(t, "not_found", header.Code)
		assert.Equal(t, "not_found", header.Kill.Outcome)
		assert.Equal(t, `Task or subagent "monitor-1" not found.`, body)
	}
}

// TestKillBackgroundToolDescriptionNamesEveryProducer pins the three id sources
// the model can address, because an id it cannot place is one it will not use.
func TestKillBackgroundToolDescriptionNamesEveryProducer(t *testing.T) {
	t.Parallel()

	declaration := NewKillBackgroundTool(nil, nil).Decl()
	assert.Contains(t, declaration.Description, "shell")
	assert.Contains(t, declaration.Description, "monitor")
	assert.Contains(t, declaration.Description, subagent.ToolName)

	// The argument's own description has to name the third producer too: a model
	// filling the field reads it rather than the Tool's summary line.
	require.NotNil(t, declaration.InputSchema)

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(declaration.InputSchema.RawJSON, &schema))
	assert.Contains(t, schema.Properties["task_id"].Description, "shell")
	assert.Contains(t, schema.Properties["task_id"].Description, "monitor")
	assert.Contains(t, schema.Properties["task_id"].Description, "subagent_id")
}

// TestKillBackgroundToolNamesKnownIDsOnNotFound pins the recovery hint: a stop
// for an id the registry does not know names the ids it does, so a model that
// lost an id can recover it.
func TestKillBackgroundToolNamesKnownIDsOnNotFound(t *testing.T) {
	t.Parallel()

	tool := NewKillBackgroundTool(
		func(context.Context, string) (background.Outcome, background.Subject, error) {
			return background.OutcomeNotFound, background.SubjectSubagent, nil
		},
		func(context.Context) []string { return []string{"monitor-1", "monitor-2"} },
	)

	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: KillBackgroundName, Args: ai.JSON(`{"task_id":"monitor-9"}`),
	})
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	_, body, err := ParseResult(text.Text)
	require.NoError(t, err)
	assert.Equal(t,
		`Task or subagent "monitor-9" not found. Known task ids: monitor-1, monitor-2.`, body)

	// A killed id still kills; the enumeration belongs to the not-found sentence.
	tool = NewKillBackgroundTool(
		func(context.Context, string) (background.Outcome, background.Subject, error) {
			return background.OutcomeKilled, background.SubjectTask, nil
		},
		func(context.Context) []string { return []string{"monitor-1"} },
	)
	parts, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-2", Name: KillBackgroundName, Args: ai.JSON(`{"task_id":"monitor-1"}`),
	})
	require.NoError(t, err)

	text, ok = parts[0].(ai.TextPart)
	require.True(t, ok)

	_, body, err = ParseResult(text.Text)
	require.NoError(t, err)
	assert.Equal(t, `Task "monitor-1" was terminated successfully.`, body)
}

// TestKnownIDsTextStaysBounded keeps a long id list from flooding the sentence.
func TestKnownIDsTextStaysBounded(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 200)
	assert.LessOrEqual(t, len(knownIDsText([]string{long, long, long, long})), maxKillKnownIDBytes+3)
	assert.Empty(t, knownIDsText([]string{strings.Repeat("y", maxKillKnownIDBytes+1)}))
}

func TestKillBackgroundToolRefusesUnusableCalls(t *testing.T) {
	t.Parallel()

	tool := NewKillBackgroundTool(
		func(context.Context, string) (background.Outcome, background.Subject, error) {
			return background.OutcomeKilled, background.SubjectTask, nil
		},
		nil,
	)

	cases := map[string]string{
		"missing task id": `{}`,
		"unknown field":   `{"task_id":"monitor-1","force":true}`,
		"wrong shape":     `{"task_id":7}`,
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := tool.Exec(t.Context(), agent.ToolCall{
				ID: "call-1", Name: KillBackgroundName, Args: ai.JSON(args),
			})
			assert.Error(t, err)
		})
	}

	// A control failure is reported, never swallowed into a success.
	failing := NewKillBackgroundTool(
		func(context.Context, string) (background.Outcome, background.Subject, error) {
			return background.OutcomeKilled, background.SubjectTask, errors.New("registry closed")
		},
		nil,
	)

	_, err := failing.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: KillBackgroundName, Args: ai.JSON(`{"task_id":"monitor-1"}`),
	})
	assert.Error(t, err)
}
