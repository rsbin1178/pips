package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerServesLineDelimitedACPOverStdio(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{}
	server, err := New(Config{
		Factory: factory,
		AgentInfo: Implementation{
			Name: "pips", Version: "test",
		},
		Limits: DefaultLimits(),
	})
	require.NoError(t, err)

	serverInput, clientOutput := io.Pipe()
	clientInput, serverOutput := io.Pipe()

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(context.Background(), serverInput, serverOutput)
	}()

	reader := bufio.NewReader(clientInput)

	_, err = clientOutput.Write([]byte("{malformed json-rpc}\n"))
	require.NoError(t, err)
	writeJSONRPCLine(t, clientOutput, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}},
	})
	initialized := readJSONRPCLine(t, reader)
	assert.InDelta(t, 1, initialized["id"], 0)
	result := requireType[map[string]any](t, initialized["result"])
	assert.InDelta(t, 1, result["protocolVersion"], 0)
	capabilities := requireType[map[string]any](t, result["agentCapabilities"])
	sessionCapabilities := requireType[map[string]any](t, capabilities["sessionCapabilities"])
	assert.Equal(t, map[string]any{}, sessionCapabilities["delete"])

	writeJSONRPCLine(t, clientOutput, map[string]any{
		"jsonrpc": "2.0", "id": "new", "method": "session/new",
		"params": map[string]any{"cwd": "/workspace", "mcpServers": []any{}},
	})
	created := readJSONRPCLine(t, reader)
	assert.Equal(t, "new", created["id"])
	createdResult := requireType[map[string]any](t, created["result"])
	assert.Equal(t, "session-1", createdResult["sessionId"])
	configOptions := requireType[[]any](t, createdResult["configOptions"])
	require.Len(t, configOptions, 2)
	modeOption := requireType[map[string]any](t, configOptions[0])
	assert.Equal(t, "mode", modeOption["id"])
	assert.Equal(t, "agent", modeOption["currentValue"])
	modelOption := requireType[map[string]any](t, configOptions[1])
	assert.Equal(t, "model", modelOption["id"])
	assert.Equal(t, "openai/model-one", modelOption["currentValue"])

	writeJSONRPCLine(t, clientOutput, map[string]any{
		"jsonrpc": "2.0", "id": "config", "method": "session/set_config_option",
		"params": map[string]any{
			"sessionId": "session-1", "configId": "mode", "value": "plan",
		},
	})
	modeChanged := readJSONRPCLine(t, reader)
	assert.Equal(t, "session/update", modeChanged["method"])
	assert.Equal(t, "current_mode_update", sessionUpdateKind(t, modeChanged))
	configChanged := readJSONRPCLine(t, reader)
	assert.Equal(t, "config_option_update", sessionUpdateKind(t, configChanged))
	configured := readJSONRPCLine(t, reader)
	assert.Equal(t, "config", configured["id"])

	writeJSONRPCLine(t, clientOutput, map[string]any{
		"jsonrpc": "2.0", "id": "model", "method": "session/set_config_option",
		"params": map[string]any{
			"sessionId": "session-1", "configId": "model", "value": "anthropic/model-two",
		},
	})
	modelChanged := readJSONRPCLine(t, reader)
	assert.Equal(t, "config_option_update", sessionUpdateKind(t, modelChanged))
	modelConfigured := readJSONRPCLine(t, reader)
	assert.Equal(t, "model", modelConfigured["id"])
	modelResult := requireType[map[string]any](t, modelConfigured["result"])
	modelConfigOptions := requireType[[]any](t, modelResult["configOptions"])
	require.Len(t, modelConfigOptions, 2)
	selectedModel := requireType[map[string]any](t, modelConfigOptions[1])
	assert.Equal(t, "anthropic/model-two", selectedModel["currentValue"])

	factory.mu.Lock()
	controller := factory.controllers["session-1"]
	factory.mu.Unlock()
	require.NotNil(t, controller)
	controller.mu.Lock()
	controller.state.ContextWindow = 200_000
	controller.state.ContextTokens = 1_234
	controller.prompt = func(context.Context, ...ai.Message) runtimeSequence {
		return eventSequence(
			coding.Event{
				Sequence: 1, SessionID: "session-1", InteractionID: "interaction-1", RunID: "run-1",
				Payload: coding.MessageDelta{
					Kind: ai.StreamMessageStart, ResponseID: "response-1",
				},
			},
			coding.Event{
				Sequence: 2, SessionID: "session-1", InteractionID: "interaction-1", RunID: "run-1",
				Payload: coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "wire answer"},
			},
			coding.Event{
				Sequence: 3, SessionID: "session-1", InteractionID: "interaction-1", RunID: "run-1",
				Payload: coding.MessageDelta{Kind: ai.StreamMessageEnd},
			},
			coding.Event{
				Sequence: 4,
				Time:     time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
				Payload: coding.InteractionCompleted{
					Outcome: coding.InteractionSucceeded, Stop: agent.StopEndTurn,
				},
			},
		)
	}
	controller.mu.Unlock()

	writeJSONRPCLine(t, clientOutput, map[string]any{
		"jsonrpc": "2.0", "id": "prompt", "method": "session/prompt",
		"params": map[string]any{
			"sessionId": "session-1", "messageId": "client-message-1",
			"prompt": []any{map[string]any{"type": "text", "text": "hello"}},
		},
	})
	message := readJSONRPCLine(t, reader)
	assert.Equal(t, "agent_message_chunk", sessionUpdateKind(t, message))
	messageUpdate := sessionUpdate(t, message)
	assert.Equal(t, "wire answer", requireType[map[string]any](t, messageUpdate["content"])["text"])
	messageID := requireType[string](t, messageUpdate["messageId"])
	assert.Regexp(t,
		`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
		messageID,
	)
	metadataUpdate := readJSONRPCLine(t, reader)
	assert.Equal(t, "session_info_update", sessionUpdateKind(t, metadataUpdate))
	usage := readJSONRPCLine(t, reader)
	assert.Equal(t, "usage_update", sessionUpdateKind(t, usage))
	usageUpdate := sessionUpdate(t, usage)
	assert.InDelta(t, 200_000, usageUpdate["size"], 0)
	assert.InDelta(t, 1_234, usageUpdate["used"], 0)
	prompted := readJSONRPCLine(t, reader)
	assert.Equal(t, "prompt", prompted["id"])
	promptResult := requireType[map[string]any](t, prompted["result"])
	assert.Equal(t, "client-message-1", promptResult["userMessageId"])

	writeJSONRPCLine(t, clientOutput, map[string]any{
		"jsonrpc": "2.0", "id": "list", "method": "session/list", "params": map[string]any{},
	})
	listed := readJSONRPCLine(t, reader)
	assert.Equal(t, "list", listed["id"])
	listedResult := requireType[map[string]any](t, listed["result"])
	assert.Equal(t, []any{}, listedResult["sessions"])

	writeJSONRPCLine(t, clientOutput, map[string]any{
		"jsonrpc": "2.0", "id": "delete", "method": "session/delete",
		"params": map[string]any{"sessionId": "session-1"},
	})
	deleted := readJSONRPCLine(t, reader)
	assert.Equal(t, "delete", deleted["id"])
	assert.Equal(t, map[string]any{}, deleted["result"])

	require.NoError(t, clientOutput.Close())

	select {
	case err := <-serveResult:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("ACP server did not stop after stdin closed")
	}

	controller.mu.Lock()
	assert.True(t, controller.closed)
	assert.Equal(t, 1, controller.closeCount)
	controller.mu.Unlock()
	require.NoError(t, clientInput.Close())
}

func sessionUpdateKind(t *testing.T, message map[string]any) string {
	t.Helper()

	return requireType[string](t, sessionUpdate(t, message)["sessionUpdate"])
}

func sessionUpdate(t *testing.T, message map[string]any) map[string]any {
	t.Helper()

	params := requireType[map[string]any](t, message["params"])

	return requireType[map[string]any](t, params["update"])
}

func writeJSONRPCLine(t *testing.T, writer io.Writer, value any) {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	encoded = append(encoded, '\n')
	_, err = writer.Write(encoded)
	require.NoError(t, err)
}

func readJSONRPCLine(t *testing.T, reader *bufio.Reader) map[string]any {
	t.Helper()

	line, err := reader.ReadBytes('\n')
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(line, &decoded))

	return decoded
}
