package tasklist_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeAndProjectLatestValidUpdate(t *testing.T) {
	t.Parallel()

	first := `{"plan":[{"step":"Inspect","status":"completed"},{"step":"Implement","status":"in_progress"}]}`
	second := `{"explanation":"verified","plan":[{"step":"Inspect","status":"completed"},{"step":"Implement","status":"completed"}]}`
	malformed := `{"plan":[],"unknown":true}`
	messages := []ai.Message{
		ai.Assistant(ai.ToolCallPart{ID: "1", Name: tasklist.ToolName, Args: ai.JSON(first)}),
		ai.Assistant(ai.ToolCallPart{ID: "2", Name: tasklist.ToolName, Args: ai.JSON(malformed)}),
		ai.Assistant(ai.ToolCallPart{ID: "3", Name: tasklist.ToolName, Args: ai.JSON(second)}),
	}

	snapshot := tasklist.Project(messages)
	assert.Equal(t, 2, snapshot.Completed)
	assert.Equal(t, 2, snapshot.Total)
	assert.Zero(t, snapshot.InProgress)

	snapshot.Items[0].Step = "mutated"
	assert.Equal(t, "Inspect", tasklist.Project(messages).Items[0].Step)
}

func TestDecodeRejectsUnsafeUpdates(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":         `{"plan":[]}`,
		"unknown":       `{"plan":[{"step":"A","status":"pending"}],"extra":true}`,
		"duplicate key": `{"plan":[{"step":"A","step":"B","status":"pending"}]}`,
		"bad status":    `{"plan":[{"step":"A","status":"running"}]}`,
		"two active":    `{"plan":[{"step":"A","status":"in_progress"},{"step":"B","status":"in_progress"}]}`,
		"blank step":    `{"plan":[{"step":" ","status":"pending"}]}`,
		"trailing json": `{"plan":[{"step":"A","status":"pending"}]} {}`,
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := tasklist.Decode([]byte(raw))
			require.Error(t, err)
		})
	}

	items := make([]tasklist.Item, tasklist.MaxItems+1)
	for index := range items {
		items[index] = tasklist.Item{Step: "A", Status: tasklist.StatusPending}
	}
	oversized, err := json.Marshal(tasklist.Update{Plan: items})
	require.NoError(t, err)
	_, err = tasklist.Decode(oversized)
	require.Error(t, err)

	longStep := strings.Repeat("x", tasklist.MaxStepBytes+1)
	encoded, err := json.Marshal(tasklist.Update{Plan: []tasklist.Item{{
		Step: longStep, Status: tasklist.StatusPending,
	}}})
	require.NoError(t, err)
	_, err = tasklist.Decode(encoded)
	require.Error(t, err)
}
