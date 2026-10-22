package openai_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatStreamRequiresFinishReason(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, stream string
		wantError    bool
	}{
		{"empty EOF", "", true},
		{"DONE only", "data: [DONE]\n\n", true},
		{"text EOF", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", true},
		{"text DONE", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: [DONE]\n\n", true},
		{"finish EOF", "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			model := newTestModel(t, serveSSE(t, tc.stream))

			resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{Messages: ai.Messages{ai.UserText("x")}}))
			if tc.wantError {
				require.ErrorIs(t, err, ai.ErrIncompleteStream)
				assert.True(t, ai.IsRetryable(err))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, "answer", resp.Text())
		})
	}
}

func TestResponsesTerminalSnapshots(t *testing.T) {
	t.Parallel()

	text := `{"type":"message","id":"msg","content":[{"type":"output_text","text":"Hello!"}]}`
	tool := `{"type":"function_call","id":"fc","call_id":"call","name":"lookup","arguments":"{}","status":"completed"}`
	terminal := `data: {"type":"response.completed","response":{"id":"resp","model":"m","status":"completed","output":[` + text + `,` + tool + `]}}` + "\n\n"

	prefix := `data: {"type":"response.created","response":{"id":"resp","model":"m"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hel"}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc","call_id":"call","name":"lookup"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{"}` + "\n\n"
	for _, tc := range []struct{ name, stream string }{
		{"final only", terminal},
		{"missing tails", prefix + terminal},
		{"item done and terminal", prefix + `data: {"type":"response.output_item.done","output_index":0,"item":` + text + `}` + "\n\n" + terminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			model := newResponsesModel(t, serveSSE(t, tc.stream))
			resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{Messages: ai.Messages{ai.UserText("x")}}))
			require.NoError(t, err)
			assert.Equal(t, "Hello!", resp.Text())
			require.Len(t, resp.ToolCalls(), 1)
			assert.JSONEq(t, `{}`, string(resp.ToolCalls()[0].Args))
			assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
		})
	}
}

func TestResponsesRejectIncompleteAndFailed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, stream string
		cause        error
		retryable    bool
	}{
		{"EOF", `data: {"type":"response.created","response":{"id":"r","model":"m"}}` + "\n\n", ai.ErrIncompleteStream, true},
		{"failed no error", `data: {"type":"response.failed","response":{"id":"r","status":"failed"}}` + "\n\n", ai.ErrResponseFailed, true},
		{"incomplete without snapshot", `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" + `data: {"type":"response.incomplete"}` + "\n\n", ai.ErrIncompleteStream, true},
		{"incomplete without status", `data: {"type":"response.incomplete","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}}` + "\n\n", ai.ErrIncompleteStream, true},
		{"incomplete conflicting status", `data: {"type":"response.incomplete","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}}` + "\n\n", ai.ErrIncompleteStream, true},
		{"filtered", `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}` + "\n\n", ai.ErrContentFiltered, false},
		{"refusal", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"No"}]}]}}` + "\n\n", ai.ErrRefused, false},
		{"unfinished tool", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call","name":"x","arguments":"{}","status":"in_progress"}]}}` + "\n\n", ai.ErrIncompleteStream, true},
		{"malformed tool", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call","name":"x","arguments":"{"}]}}` + "\n\n", ai.ErrIncompleteStream, true},
		{"unclosed tool", `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call","name":"x","arguments":"{}"}}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n", ai.ErrIncompleteStream, true},
		{"conflicting text", `data: {"type":"response.output_text.delta","output_index":0,"delta":"old"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"new"}]}]}}` + "\n\n", ai.ErrIncompleteStream, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			model := newResponsesModel(t, serveSSE(t, tc.stream))

			var ended bool

			var failure error

			for ev, err := range model.Stream(t.Context(), ai.Request{Messages: ai.Messages{ai.UserText("x")}}) {
				if ev.Type == ai.StreamMessageEnd {
					ended = true
				}

				if err != nil {
					failure = err
				}
			}

			require.ErrorIs(t, failure, tc.cause)
			assert.Equal(t, tc.retryable, ai.IsRetryable(failure))
			assert.False(t, ended)
		})
	}
}

func TestResponsesLengthWinsOverPartialTools(t *testing.T) {
	t.Parallel()

	body := `{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"call","name":"x","arguments":"{","status":"incomplete"}]}`
	model := newResponsesModel(t, serveSSE(t, `data: {"type":"response.incomplete","response":`+body+`}`+"\n\n"))
	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{Messages: ai.Messages{ai.UserText("x")}}))
	require.NoError(t, err)
	assert.Equal(t, ai.FinishLength, resp.FinishReason)
	require.Len(t, resp.ToolCalls(), 1)
	generated := newResponsesModel(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
	resp, err = generated.Generate(t.Context(), ai.Request{Messages: ai.Messages{ai.UserText("x")}})
	require.NoError(t, err)
	assert.Equal(t, ai.FinishLength, resp.FinishReason)
}

func TestResponsesGenerateFailedWithoutError(t *testing.T) {
	t.Parallel()
	model := newResponsesModel(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"failed","output":[]}`))
	})
	_, err := model.Generate(t.Context(), ai.Request{Messages: ai.Messages{ai.UserText("x")}})
	require.ErrorIs(t, err, ai.ErrResponseFailed)
}

func TestResponseFailureSummaryDoesNotMarshalCause(t *testing.T) {
	t.Parallel()

	failure := ai.NewResponseError(&ai.Error{Message: "private-prompt", Raw: []byte("secret-body")}, nil, false)
	body, err := json.Marshal(ai.NewRetryNotice(1, 1, 0, failure))
	require.NoError(t, err)
	assert.NotContains(t, string(body), "private-prompt")
	assert.NotContains(t, string(body), "secret-body")
	assert.NotContains(t, strings.ToLower(string(body)), "cause")
}

func TestChatRejectsExplicitNonAnswerAndInvalidTools(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, delta, finish string
		cause               error
		retryable           bool
	}{
		{"filtered", `{}`, "content_filter", ai.ErrContentFiltered, false},
		{"refused", `{"refusal":"declined"}`, "stop", ai.ErrRefused, false},
		{"invalid tool JSON", `{"tool_calls":[{"index":0,"id":"call","function":{"name":"x","arguments":"{"}}]}`, "tool_calls", ai.ErrIncompleteStream, true},
		{"missing tool identity", `{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`, "tool_calls", ai.ErrIncompleteStream, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stream := `data: {"choices":[{"delta":` + tc.delta + `,"finish_reason":"` + tc.finish + `"}]}` + "\n\ndata: [DONE]\n\n"
			model := newTestModel(t, serveSSE(t, stream))
			_, err := ai.Collect(model.Stream(t.Context(), ai.Request{Messages: ai.Messages{ai.UserText("x")}}))
			require.ErrorIs(t, err, tc.cause)
			assert.Equal(t, tc.retryable, ai.IsRetryable(err))
		})
	}
}
