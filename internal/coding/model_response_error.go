package coding

import (
	"context"
	"errors"

	"github.com/rsbin1178/pips/ai"
)

//nolint:gocyclo // Stable failure classes are mapped explicitly after cancellation precedence.
func modelRuntimeError(runErr error, outcome InteractionOutcome) RuntimeError {
	result := RuntimeError{Code: "run_failed", Message: "Agent run failed", Fatal: true}
	if outcome == InteractionCanceled || errors.Is(runErr, context.Canceled) ||
		errors.Is(runErr, errConsumerStopped) {
		result.Code, result.Message = "run_canceled", "Agent run was canceled"

		return result
	}

	if errors.Is(runErr, context.DeadlineExceeded) {
		result.Code, result.Message = "run_deadline_exceeded", "The request deadline was reached"

		return result
	}

	var responseErr *ai.ResponseError
	if errors.As(runErr, &responseErr) {
		result.Response = modelResponseFailure(responseErr)
	}

	switch {
	case responseErr != nil && responseErr.FinishReason == ai.FinishLength:
		result.Code = "model_output_limit"
		result.Message = "The model reached its output limit without a complete answer; increase the output limit and try again"
	case errors.Is(runErr, ai.ErrIncompleteStream):
		result.Code = "model_stream_incomplete"
		result.Message = "The model stream ended before completion and recovery stopped; try again or check the provider connection"
	case errors.Is(runErr, ai.ErrEmptyResponse):
		result.Code = "model_response_empty"

		result.Message = "The model returned no answer and recovery stopped; try again or select another model"
		if responseErr != nil && responseErr.HasReasoning {
			result.Code = "model_response_reasoning_only"
			result.Message = "The model returned reasoning without an answer and recovery stopped; try again or select another model"
		}
	case errors.Is(runErr, ai.ErrContentFiltered):
		result.Code, result.Message = "model_content_filtered", "The provider filtered the response; revise the request"
	case errors.Is(runErr, ai.ErrRefused):
		result.Code, result.Message = "model_response_refused", "The provider declined to answer; revise the request"
	case errors.Is(runErr, ai.ErrResponseFailed):
		result.Code, result.Message = "model_response_failed", "The provider could not complete the response; try again"
	}

	return result
}

func modelResponseFailure(response *ai.ResponseError) *ModelResponseFailure {
	if response == nil {
		return nil
	}

	usage := tokenUsageFromAI(response.Usage)
	usage.InputTokens = max(0, usage.InputTokens)
	usage.OutputTokens = max(0, usage.OutputTokens)
	usage.ReasoningTokens = max(0, usage.ReasoningTokens)
	usage.CachedInputTokens = max(0, usage.CachedInputTokens)
	usage.CacheWriteTokens = max(0, usage.CacheWriteTokens)

	return &ModelResponseFailure{
		Kind:     modelResponseFailureKind(response),
		Provider: ai.Provider(responseDiagnosticID(string(response.Provider))),
		Model:    responseDiagnosticID(response.Model), ResponseID: responseDiagnosticID(response.ResponseID),
		TerminalObserved: response.TerminalObserved, FinishReason: responseDiagnosticFinish(response.FinishReason),
		Status: responseDiagnosticStatus(response.Status), TextBytes: max(0, response.TextBytes),
		ToolCalls: max(0, response.ToolCalls), HasReasoning: response.HasReasoning, Usage: usage,
	}
}

// Reject free-form identifiers rather than allowing error text into diagnostics.
func responseDiagnosticID(value string) string {
	if len(value) > 128 {
		return ""
	}

	for _, char := range value {
		if responseDiagnosticIDChar(char) {
			continue
		}

		return ""
	}

	return value
}

func responseDiagnosticIDChar(char rune) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9' || char == '-' || char == '_' ||
		char == '.' || char == '/' || char == ':'
}

func responseDiagnosticFinish(reason ai.FinishReason) ai.FinishReason {
	switch reason {
	case ai.FinishStop, ai.FinishLength, ai.FinishToolCalls, ai.FinishContentFilter, ai.FinishOther:
		return reason
	default:
		return ""
	}
}

func responseDiagnosticStatus(status string) string {
	switch status {
	case "completed", "incomplete", "failed", "cancelled", "queued", "in_progress":
		return status
	default:
		return ""
	}
}

func validateModelResponseFailure(value *ModelResponseFailure) error {
	if value == nil {
		return nil
	}

	if value.Turn < 0 || value.Attempt < 0 || value.MaxRetries < value.Attempt ||
		!validModelResponseFailureKind(value.Kind) ||
		!validModelResponseIdentity(value) ||
		responseDiagnosticFinish(value.FinishReason) != value.FinishReason ||
		responseDiagnosticStatus(value.Status) != value.Status || value.TextBytes < 0 || value.ToolCalls < 0 ||
		!validTokenUsage(value.Usage) {
		return errors.New("invalid model response diagnostic")
	}

	return nil
}

func validModelResponseIdentity(value *ModelResponseFailure) bool {
	return responseDiagnosticID(string(value.Provider)) == string(value.Provider) &&
		responseDiagnosticID(value.Model) == value.Model && responseDiagnosticID(value.ResponseID) == value.ResponseID
}

func modelRetryDiagnostic(retry ModelRetry) IntegrationDiagnostic {
	response := cloneModelResponseFailure(retry.Response)
	response.Turn, response.Attempt, response.MaxRetries = retry.Turn, retry.Attempt, retry.MaxRetries

	return IntegrationDiagnostic{
		Component: "model", Code: response.Kind, Response: response,
	}
}

func modelResponseFailureKind(response *ai.ResponseError) string {
	switch {
	case response.FinishReason == ai.FinishLength:
		return "output_limit"
	case errors.Is(response, ai.ErrIncompleteStream):
		return "incomplete_stream"
	case errors.Is(response, ai.ErrEmptyResponse):
		if response.HasReasoning {
			return "reasoning_only"
		}

		return "empty_response"
	case errors.Is(response, ai.ErrContentFiltered):
		return "content_filtered"
	case errors.Is(response, ai.ErrRefused):
		return "response_refused"
	default:
		return "response_failed"
	}
}

func validModelResponseFailureKind(kind string) bool {
	switch kind {
	case "output_limit", "incomplete_stream", "empty_response", "reasoning_only",
		"content_filtered", "response_refused", "response_failed":
		return true
	default:
		return false
	}
}

func cloneRuntimeError(value RuntimeError) RuntimeError {
	value.Response = cloneModelResponseFailure(value.Response)

	return value
}

func cloneModelResponseFailure(value *ModelResponseFailure) *ModelResponseFailure {
	if value == nil {
		return nil
	}

	cloned := *value

	return &cloned
}

func modelResponseFailureBytes(value *ModelResponseFailure) int {
	if value == nil {
		return 0
	}

	return 128 + len(value.Provider) + len(value.Model) + len(value.ResponseID)
}
