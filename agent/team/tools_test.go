package team

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolsetsExposeScopedCapabilities(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))

	memberTools, err := NewMemberToolset(runtime.engine, team.ID, "worker")
	require.NoError(t, err)
	leadTools, err := NewLeadToolset(runtime.engine, team.ID, "lead")
	require.NoError(t, err)
	_, err = NewLeadToolset(runtime.engine, team.ID, "worker")
	require.ErrorIs(t, err, ErrUnauthorized)

	memberNames := toolNames(memberTools.Tools())
	leadNames := toolNames(leadTools.Tools())

	assert.Len(t, memberNames, 8)
	assert.Len(t, leadNames, 16)
	assert.NotContains(t, memberNames, "team_create_task")
	assert.Contains(t, leadNames, "team_create_task")
	assert.NotContains(t, leadNames, "team_register_member")

	for _, tool := range leadTools.Tools() {
		_, parallel := tool.(agent.ConcurrencySafe)
		assert.False(t, parallel)

		schema, marshalErr := json.Marshal(tool.Decl().InputSchema)
		require.NoError(t, marshalErr)
		assert.NotContains(t, string(schema), "expected_revision")
		assert.NotContains(t, string(schema), "command_id")
		assert.NotContains(t, string(schema), "actor")
		assert.NotContains(t, string(schema), "sender_id")
	}
}

func TestLeadCreateTaskToolDerivesIDAndReplaysToolCall(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := createTestTeam(t, runtime)
	toolset, err := NewLeadToolset(runtime.engine, team.ID, "lead")
	require.NoError(t, err)
	tool := findTool(t, toolset.Tools(), "team_create_task")

	call := agent.ToolCall{
		ID: "provider-call-1", Name: "team_create_task",
		Args: ai.JSON(`{"title":"Investigate","attempt_limit":2}`),
	}
	first, err := tool.Exec(t.Context(), call)
	require.NoError(t, err)
	require.Len(t, first, 1)

	second, err := tool.Exec(t.Context(), call)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	loaded, err := runtime.engine.Get(t.Context(), team.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Tasks, 1)
	assert.Contains(t, string(loaded.Tasks[0].ID), "task-")
	assert.Equal(t, Revision(2), loaded.Revision)

	call.Args = ai.JSON(`{"title":"Different","attempt_limit":2}`)
	_, err = tool.Exec(t.Context(), call)
	require.ErrorIs(t, err, ErrCommandConflict)
}

func TestMemberMessageToolBindsSenderAndRequiresCallID(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))
	toolset, err := NewMemberToolset(runtime.engine, team.ID, "worker")
	require.NoError(t, err)
	tool := findTool(t, toolset.Tools(), "team_send_message")

	_, err = tool.Exec(t.Context(), agent.ToolCall{
		Name: "team_send_message",
		Args: ai.JSON(`{"recipient_id":"lead","body":{"text":"done"}}`),
	})
	require.ErrorIs(t, err, ErrInvalid)

	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "message-call", Name: "team_send_message",
		Args: ai.JSON(`{"recipient_id":"lead","body":{"text":"done"}}`),
	})
	require.NoError(t, err)

	loaded, err := runtime.engine.Get(t.Context(), team.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	assert.Equal(t, MemberID("worker"), loaded.Messages[0].SenderID)
	assert.Equal(t, MemberID("lead"), loaded.Messages[0].RecipientID)
}

type teamToolModel struct {
	mu   sync.Mutex
	turn int
}

func (model *teamToolModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	model.mu.Lock()
	defer model.mu.Unlock()

	model.turn++
	if model.turn == 1 {
		return &ai.Response{
			Message: ai.Message{
				Role: ai.RoleAssistant,
				Parts: []ai.Part{ai.ToolCallPart{
					ID: "agent-call-1", Name: "team_create_task",
					Args: ai.JSON(`{"title":"Agent-created task","attempt_limit":1}`),
				}},
			},
			FinishReason: ai.FinishToolCalls,
		}, nil
	}

	return &ai.Response{
		Message: ai.AssistantText("task created"), FinishReason: ai.FinishStop,
	}, nil
}

func (*teamToolModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(func(ai.StreamEvent, error) bool) {}
}

func (*teamToolModel) Provider() ai.Provider { return "team-test" }
func (*teamToolModel) ModelID() string       { return "team-test-1" }
func (*teamToolModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func TestLeadToolsetRunsInsideAgentLoop(t *testing.T) {
	t.Parallel()

	runtime := newMemoryTestRuntime(t)
	team := createTestTeam(t, runtime)
	toolset, err := NewLeadToolset(runtime.engine, team.ID, "lead")
	require.NoError(t, err)

	worker, err := agent.New(&teamToolModel{}, agent.WithTools(toolset.Tools()...))
	require.NoError(t, err)
	result, err := worker.Run(t.Context(), agent.NewSession(), ai.UserText("Create the task."))
	require.NoError(t, err)
	assert.Equal(t, "task created", result.Text())

	loaded, err := runtime.engine.Get(t.Context(), team.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Tasks, 1)
	assert.Equal(t, "Agent-created task", loaded.Tasks[0].Title)
}

func toolNames(tools []agent.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Decl().Name)
	}

	return names
}

func findTool(t *testing.T, tools []agent.Tool, name string) agent.Tool {
	t.Helper()

	for _, tool := range tools {
		if tool.Decl().Name == name {
			return tool
		}
	}

	require.FailNow(t, "tool not found", name)

	return nil
}
