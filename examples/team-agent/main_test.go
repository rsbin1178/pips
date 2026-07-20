package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRunCompletesTeamAgainstChatCompletionsEndpoint(t *testing.T) {
	t.Parallel()

	responses := []string{
		"Research identified four supported capability claims and one explicit runtime boundary.",
		"Pips next adds bounded agent execution, durable sessions, recoverable continuations, " +
			"and Team coordination. Scheduling remains application-owned.",
		"Pips next: durable building blocks for Go agent applications. It provides bounded runs, " +
			"independent session state, recoverable continuation boundaries, and Team coordination " +
			"while leaving provisioning and scheduling to the application.",
	}

	server, recorder := newTestChatServer(t, responses)

	var output bytes.Buffer

	err := run(t.Context(), modelConfig{
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	}, &output)
	if err != nil {
		t.Fatalf("run example: %v", err)
	}

	requests := recorder.snapshot()
	if len(requests) != len(responses) {
		t.Fatalf("model calls = %d, want %d", len(requests), len(responses))
	}

	for index, request := range requests {
		if request.path != "/v1/chat/completions" {
			t.Errorf("request %d path = %q", index, request.path)
		}

		if request.authorization != "Bearer test-key" {
			t.Errorf("request %d authorization = %q", index, request.authorization)
		}

		if request.body["model"] != "test-model" {
			t.Errorf("request %d model = %v", index, request.body["model"])
		}

		if request.body["stream"] != true {
			t.Errorf("request %d stream = %v", index, request.body["stream"])
		}

		if tools, ok := request.body["tools"].([]any); !ok || len(tools) != 3 {
			t.Errorf(
				"request %d tools = %T(%v), want 3 tools",
				index,
				request.body["tools"],
				request.body["tools"],
			)
		}
	}

	for _, text := range []string{
		"child=terminal",
		"Team status: completed",
		"turn-1-research: completed",
		"turn-1-draft: completed",
		"Messages: 2",
		"Final briefing:",
	} {
		if !strings.Contains(output.String(), text) {
			t.Errorf("output does not contain %q:\n%s", text, output.String())
		}
	}
}

func TestRunTerminalKeepsMemberSessionsAcrossTurns(t *testing.T) {
	t.Parallel()

	responses := []string{
		"research one",
		"draft one",
		"answer one",
		"research two using prior context",
		"draft two using prior context",
		"answer two",
	}
	server, recorder := newTestChatServer(t, responses)

	input := strings.NewReader("first request\n/status\nfollow up request\n/exit\n")

	var output bytes.Buffer

	var diagnostics bytes.Buffer

	err := runTerminal(t.Context(), modelConfig{
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	}, input, &output, &diagnostics)
	if err != nil {
		t.Fatalf("run terminal: %v", err)
	}

	requests := recorder.snapshot()
	if len(requests) != len(responses) {
		t.Fatalf("model calls = %d, want %d", len(requests), len(responses))
	}

	firstResearchMessages := requestMessages(t, requests[0])

	secondResearchMessages := requestMessages(t, requests[3])
	if len(secondResearchMessages) <= len(firstResearchMessages) {
		t.Errorf(
			"second researcher context has %d messages, want more than first turn's %d",
			len(secondResearchMessages),
			len(firstResearchMessages),
		)
	}

	for _, text := range []string{
		"[researcher] research one",
		"[writer] draft one",
		"[lead] answer one",
		"[researcher] research two using prior context",
		"[writer] draft two using prior context",
		"[lead] answer two",
	} {
		if !strings.Contains(output.String(), text) {
			t.Errorf("terminal output does not contain %q:\n%s", text, output.String())
		}
	}

	for _, text := range []string{
		"第 1 轮",
		"第 2 轮",
		"status=active",
		"status=completed",
		"turns=2",
	} {
		if !strings.Contains(diagnostics.String(), text) {
			t.Errorf("terminal diagnostics do not contain %q:\n%s", text, diagnostics.String())
		}
	}
}

func newTestChatServer(
	t *testing.T,
	responses []string,
) (*httptest.Server, *requestRecorder) {
	t.Helper()

	recorder := &requestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}

		call := recorder.add(request.URL.Path, request.Header.Get("Authorization"), body)
		if call > len(responses) {
			http.Error(writer, "unexpected extra model call", http.StatusInternalServerError)
			return
		}

		if stream, _ := body["stream"].(bool); stream {
			writeTestChatStream(writer, call, responses[call-1])

			return
		}

		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"id":    fmt.Sprintf("chatcmpl-%d", call),
			"model": "test-model",
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": responses[call-1],
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 5,
			},
		})
	}))
	t.Cleanup(server.Close)

	return server, recorder
}

func writeTestChatStream(writer http.ResponseWriter, call int, response string) {
	writer.Header().Set("Content-Type", "text/event-stream")

	middle := len(response) / 2
	for _, content := range []string{response[:middle], response[middle:]} {
		writeTestSSE(writer, map[string]any{
			"id":     fmt.Sprintf("chatcmpl-%d", call),
			"object": "chat.completion.chunk",
			"model":  "test-model",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"content": content,
				},
				"finish_reason": nil,
			}},
		})
	}

	writeTestSSE(writer, map[string]any{
		"id":     fmt.Sprintf("chatcmpl-%d", call),
		"object": "chat.completion.chunk",
		"model":  "test-model",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	})
	writeTestSSE(writer, map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", call),
		"object":  "chat.completion.chunk",
		"model":   "test-model",
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
		},
	})
	_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
}

func writeTestSSE(writer io.Writer, value any) {
	data, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", data)
}

func requestMessages(t *testing.T, request recordedRequest) []any {
	t.Helper()

	messages, ok := request.body["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %T(%v)", request.body["messages"], request.body["messages"])
	}

	return messages
}

type recordedRequest struct {
	path          string
	authorization string
	body          map[string]any
}

type requestRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (recorder *requestRecorder) add(path, authorization string, body map[string]any) int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	recorder.requests = append(recorder.requests, recordedRequest{
		path:          path,
		authorization: authorization,
		body:          body,
	})

	return len(recorder.requests)
}

func (recorder *requestRecorder) snapshot() []recordedRequest {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	return append([]recordedRequest(nil), recorder.requests...)
}
