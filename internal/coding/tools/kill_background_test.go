package tools

import (
	"context"
	"errors"
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
	})
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

func TestKillBackgroundToolRefusesUnusableCalls(t *testing.T) {
	t.Parallel()

	tool := NewKillBackgroundTool(func(context.Context, string) (background.Outcome, error) {
		return background.OutcomeKilled, nil
	})

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
	})

	_, err := failing.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: KillBackgroundName, Args: ai.JSON(`{"task_id":"monitor-1"}`),
	})
	assert.Error(t, err)
}
