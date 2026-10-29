package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelRuntimeErrorClassifiesResponseFailuresWithoutProviderContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cause  error
		reason ai.FinishReason
		think  bool
		code   string
	}{
		{name: "empty", cause: ai.ErrEmptyResponse, code: "model_response_empty"},
		{name: "reasoning", cause: ai.ErrEmptyResponse, think: true, code: "model_response_reasoning_only"},
		{name: "incomplete", cause: ai.ErrIncompleteStream, code: "model_stream_incomplete"},
		{name: "length", cause: ai.ErrEmptyResponse, reason: ai.FinishLength, code: "model_output_limit"},
		{name: "filtered", cause: ai.ErrContentFiltered, code: "model_content_filtered"},
		{name: "refused", cause: ai.ErrRefused, code: "model_response_refused"},
		{name: "failed", cause: ai.ErrResponseFailed, code: "model_response_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response := &ai.ResponseError{
				Cause:    fmt.Errorf("private provider body: %w", test.cause),
				Provider: ai.ProviderOpenAI, Model: "model-1", ResponseID: "response-1",
				FinishReason: test.reason, HasReasoning: test.think,
			}
			result := modelRuntimeError(fmt.Errorf("private wrapper: %w", response), InteractionFailed)
			assert.Equal(t, test.code, result.Code)
			require.NotNil(t, result.Response)
			assert.NotEmpty(t, result.Message)

			event := newTestEvent(EventError, result)
			encoded, err := MarshalEvent(event)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "private")
			decoded, err := UnmarshalEvent(encoded)
			require.NoError(t, err)
			assert.Equal(t, result, decoded.Payload)
		})
	}
}

func TestModelResponseDiagnosticsSanitizeValidateAndPreserveDisclosure(t *testing.T) {
	t.Parallel()

	response := modelResponseFailure(&ai.ResponseError{
		Cause: ai.ErrIncompleteStream, Provider: ai.ProviderOpenAI,
		Model: "private model body", ResponseID: strings.Repeat("r", 129),
		Status: "private provider status", FinishReason: "private finish reason",
		TextBytes: -1, ToolCalls: -1, Usage: ai.Usage{OutputTokens: -1},
	})
	assert.Empty(t, response.Model)
	assert.Empty(t, response.ResponseID)
	assert.Empty(t, response.Status)
	assert.Empty(t, response.FinishReason)
	assert.Zero(t, response.TextBytes)
	assert.Zero(t, response.Usage.OutputTokens)

	response.ResponseID = "private-response-id"
	for _, event := range []Event{
		newTestEvent(EventError, RuntimeError{Code: "model_stream_incomplete", Response: response}),
		newTestEvent(EventIntegrationDiagnostic, IntegrationDiagnostic{Component: "model", Code: "incomplete_stream", Response: response}),
		newTestEvent(EventModelRetry, ModelRetry{Attempt: 1, MaxRetries: 2, Response: response}),
	} {
		safe, err := Project(event, DisclosureSafe)
		require.NoError(t, err)
		encoded, err := json.Marshal(safe)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "private-response-id")
		assert.NotContains(t, string(encoded), "terminal_observed")

		telemetry, err := Telemetry(event)
		require.NoError(t, err)
		encoded, err = json.Marshal(telemetry)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "private-response-id")

		cloned := cloneEvent(event)
		response.TextBytes++

		assert.NotEqual(t, event.Payload, cloned.Payload, "event clones detach diagnostic pointers")
	}

	bad := *response
	bad.Status = "untrusted status"
	require.ErrorIs(t, ValidateEvent(newTestEvent(EventError,
		RuntimeError{Code: "failure", Response: &bad})), ErrInvalidEvent)

	legacy := newTestEvent(EventError, RuntimeError{Code: "run_failed", Message: "Agent run failed", Fatal: true})
	encoded, err := MarshalEvent(legacy)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "response")
	_, err = UnmarshalEvent(encoded)
	require.NoError(t, err)
}

func TestModelRuntimeErrorCancellationPrecedesResponseFailure(t *testing.T) {
	t.Parallel()

	failure := &ai.ResponseError{Cause: ai.ErrEmptyResponse, HasReasoning: true}
	for _, test := range []struct {
		cause error
		code  string
	}{
		{cause: context.Canceled, code: "run_canceled"},
		{cause: context.DeadlineExceeded, code: "run_deadline_exceeded"},
	} {
		result := modelRuntimeError(errors.Join(failure, test.cause), InteractionFailed)
		assert.Equal(t, test.code, result.Code)
		assert.Nil(t, result.Response, "a canceled request must not look like a model failure")
	}
}

func TestModelResponseDiagnosticKeepsRetryVisibleUntilTheNextAttempt(t *testing.T) {
	t.Parallel()

	response := modelResponseFailure(&ai.ResponseError{Cause: ai.ErrEmptyResponse, TerminalObserved: true})
	notice := ModelRetry{Turn: 1, Attempt: 1, MaxRetries: 2, Response: response}
	diagnostic := modelRetryDiagnostic(notice)
	events := retryEvents(
		newTestEvent(EventModelRetry, notice),
		newTestEvent(EventIntegrationDiagnostic, diagnostic),
	)
	state := reduceAll(t, events)
	require.True(t, state.Retry.Active, "persisting the failed attempt must not hide the active wait")
	require.Len(t, state.Diagnostics, 1)

	clone := state.Clone()
	clone.Diagnostics[0].Response.TextBytes = 91
	assert.Zero(t, state.Diagnostics[0].Response.TextBytes)
	diagnostic.Response.TextBytes = 37
	assert.Zero(t, state.Diagnostics[0].Response.TextBytes, "reducer takes ownership of diagnostic data")

	resumed := reduceAll(t, append(events,
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "answer"})))
	assert.False(t, resumed.Retry.Active)
}
