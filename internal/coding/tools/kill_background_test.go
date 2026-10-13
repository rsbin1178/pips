package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func killToolResult(t *testing.T, outcome background.Outcome, err error) (ResultHeader, string) {
	t.Helper()

	tool := NewKillBackgroundTool(func(context.Context, string) (background.Outcome, error) {
		return outcome, err
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

func TestKillBackgroundToolReportsTheRegistryVocabulary(t *testing.T) {
	t.Parallel()

	header, body := killToolResult(t, background.OutcomeKilled, nil)
	assert.True(t, header.OK)
	require.NotNil(t, header.Kill)
	assert.Equal(t, "monitor-1", header.Kill.TaskID)
	assert.Equal(t, "killed", header.Kill.Outcome)
	assert.Equal(t, `Task "monitor-1" was terminated successfully.`, body)

	header, body = killToolResult(t, background.OutcomeAlreadyExited, nil)
	assert.True(t, header.OK)
	assert.Equal(t, "already_exited", header.Kill.Outcome)
	assert.Equal(t, `Task "monitor-1" had already completed.`, body)

	// An unknown id is a failed call: the model must not read it as a stop.
	header, body = killToolResult(t, background.OutcomeNotFound, nil)
	assert.False(t, header.OK)
	assert.Equal(t, "not_found", header.Code)
	assert.Equal(t, "not_found", header.Kill.Outcome)
	assert.Equal(t, `Task "monitor-1" not found.`, body)
}

// TestKillBackgroundToolNamesKnownIDsOnNotFound pins the recovery hint: a stop
// for an id the registry does not know names the ids it does, so a model that
// lost an id can recover it.
func TestKillBackgroundToolNamesKnownIDsOnNotFound(t *testing.T) {
	t.Parallel()

	tool := NewKillBackgroundTool(
		func(context.Context, string) (background.Outcome, error) {
			return background.OutcomeNotFound, nil
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
	assert.Equal(t, `Task "monitor-9" not found. Known task ids: monitor-1, monitor-2.`, body)

	// A killed id still kills; the enumeration belongs to the not-found sentence.
	tool = NewKillBackgroundTool(
		func(context.Context, string) (background.Outcome, error) {
			return background.OutcomeKilled, nil
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

	tool := NewKillBackgroundTool(func(context.Context, string) (background.Outcome, error) {
		return background.OutcomeKilled, nil
	}, nil)

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
	failing := NewKillBackgroundTool(func(context.Context, string) (background.Outcome, error) {
		return background.OutcomeKilled, errors.New("registry closed")
	}, nil)

	_, err := failing.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: KillBackgroundName, Args: ai.JSON(`{"task_id":"monitor-1"}`),
	})
	assert.Error(t, err)
}
