package ai

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Sentinel errors for common failure classes. Provider adapters wrap these so
// callers can branch with [errors.Is] regardless of provider:
//
//	if errors.Is(err, ai.ErrRateLimited) { ... }
var (
	// ErrUnsupported means the model or provider does not support the
	// requested capability (for example image generation on Anthropic).
	ErrUnsupported = errors.New("ai: capability not supported")
	// ErrAuth means authentication failed (HTTP 401/403).
	ErrAuth = errors.New("ai: authentication failed")
	// ErrRateLimited means the provider rate-limited the request (HTTP 429).
	ErrRateLimited = errors.New("ai: rate limited")
	// ErrOverloaded means the provider is temporarily overloaded (HTTP
	// 500-599, or Anthropic's 529).
	ErrOverloaded = errors.New("ai: provider overloaded")
	// ErrInvalidRequest means the provider rejected the request as malformed
	// (HTTP 400/404/422).
	ErrInvalidRequest = errors.New("ai: invalid request")
	// ErrInvalidMessage means a portable message has an unsupported concrete
	// form, role-specific content, or sequence position.
	ErrInvalidMessage = errors.New("ai: invalid message")
	// ErrUnsupportedMedia means a provider wire shape cannot carry a media part
	// (image or file) from a tool result, so forwarding the request would
	// silently drop it. It matches [ErrUnsupported] because it is the same
	// "cannot be expressed" class; adapters wrap it in
	// [UnsupportedMediaError] to name the provider and the part kind.
	ErrUnsupportedMedia = fmt.Errorf("%w: media cannot be represented", ErrUnsupported)
	// ErrStreamIdle means a streaming response delivered bytes and then stayed
	// silent past the client's idle bound, so the stream was aborted. It is a
	// failure class of its own rather than a context deadline because both
	// recovery layers act on retryable errors: a parked stream that never
	// fails is the one interruption nothing else can reach.
	ErrStreamIdle = errors.New("ai: stream idle")
	// ErrResponseTimeout means a streaming request reached the peer but its
	// response headers never arrived within the client's bound, so the request
	// was abandoned. Like [ErrStreamIdle] it is a failure class of its own
	// rather than a context deadline, because both recovery layers act on
	// retryable errors: a peer that never starts answering is worth another
	// attempt, while a deadline the caller set is not.
	ErrResponseTimeout = errors.New("ai: response timed out")
	// ErrIncompleteStream means the provider ended before a complete response.
	ErrIncompleteStream = errors.New("ai: incomplete stream")
	// ErrEmptyResponse means a completed response has no deliverable output.
	ErrEmptyResponse = errors.New("ai: empty response")
	// ErrContentFiltered means output was blocked by a content filter.
	ErrContentFiltered = errors.New("ai: response content filtered")
	// ErrRefused means the provider explicitly refused the request.
	ErrRefused = errors.New("ai: request refused")
	// ErrResponseTruncated means the output limit left no usable answer.
	ErrResponseTruncated = errors.New("ai: output token limit reached")
	// ErrResponseFailed means the provider reported a failed response.
	ErrResponseFailed = errors.New("ai: response failed")
	// ErrMalformedStructuredOutput means a schema-constrained answer was not one
	// JSON object. Callers treat it as an answer of the wrong shape rather than a
	// transport failure.
	ErrMalformedStructuredOutput = errors.New("ai: structured output is not one JSON object")
)

// ResponseError describes a rejected model response without retaining its content.
// Error returns a stable cause; identifying fields are available to local diagnostics.
type ResponseError struct {
	Cause            error `json:"-"`
	Provider         Provider
	Model            string
	ResponseID       string
	TerminalObserved bool
	FinishReason     FinishReason
	Status           string
	TextBytes        int
	ToolCalls        int
	HasReasoning     bool
	Usage            Usage
}

// NewResponseError summarizes a response without copying text or tool arguments.
func NewResponseError(cause error, resp *Response, terminalObserved bool) *ResponseError {
	e := &ResponseError{Cause: cause, TerminalObserved: terminalObserved}
	if resp == nil {
		return e
	}

	e.Provider, e.Model, e.ResponseID = resp.Provider, resp.Model, resp.ID

	e.FinishReason, e.Usage = resp.FinishReason, resp.Usage
	for _, part := range resp.Message.Parts {
		switch p := part.(type) {
		case TextPart:
			e.TextBytes += len(p.Text)
		case ToolCallPart:
			e.ToolCalls++
		case ReasoningPart:
			e.HasReasoning = true
		}
	}

	return e
}

// Error implements error using only a provider-neutral failure class.
func (e *ResponseError) Error() string {
	switch {
	case e.FinishReason == FinishLength || errors.Is(e.Cause, ErrResponseTruncated):
		return "ai: output token limit reached"
	case errors.Is(e.Cause, ErrIncompleteStream):
		return ErrIncompleteStream.Error()
	case errors.Is(e.Cause, ErrEmptyResponse):
		if e.HasReasoning {
			return "ai: reasoning-only response"
		}

		return ErrEmptyResponse.Error()
	case errors.Is(e.Cause, ErrContentFiltered):
		return ErrContentFiltered.Error()
	case errors.Is(e.Cause, ErrRefused):
		return ErrRefused.Error()
	default:
		return ErrResponseFailed.Error()
	}
}

// Unwrap exposes the response failure class to errors.Is.
func (e *ResponseError) Unwrap() error { return e.Cause }

// UnsupportedMediaError names the provider and the tool-result part kind a wire
// shape cannot express. Adapters return it, wrapping [ErrUnsupportedMedia],
// instead of dropping the media from the request.
type UnsupportedMediaError struct {
	// Provider is the adapter that cannot express the part.
	Provider Provider
	// Kind is the part kind, for example "image" or "file".
	Kind string
}

// NewUnsupportedMediaError builds the error for a part a provider's wire shape
// cannot carry.
func NewUnsupportedMediaError(provider Provider, part Part) *UnsupportedMediaError {
	return &UnsupportedMediaError{Provider: provider, Kind: PartKind(part)}
}

// Error implements the error interface.
func (e *UnsupportedMediaError) Error() string {
	return fmt.Sprintf("ai: %s cannot express %s tool-result media", e.Provider, e.Kind)
}

// Unwrap exposes [ErrUnsupportedMedia] (and through it [ErrUnsupported]) to
// [errors.Is].
func (e *UnsupportedMediaError) Unwrap() error { return ErrUnsupportedMedia }

// Error is a structured provider error. Adapters return it (wrapped around a
// sentinel) for non-2xx responses. Extract it with [errors.As]:
//
//	var apiErr *ai.Error
//	if errors.As(err, &apiErr) { log.Print(apiErr.StatusCode) }
type Error struct {
	// Provider is the adapter that produced the error.
	Provider Provider
	// StatusCode is the HTTP status, or 0 for transport/decoding errors.
	StatusCode int
	// Type is the provider's error type string (for example
	// "rate_limit_error"), when present.
	Type string
	// Code is the provider's error code, when present.
	Code string
	// Message is the human-readable error message.
	Message string
	// RetryAfter is the delay requested by a Retry-After header, when present.
	RetryAfter time.Duration
	// Raw is the provider's error body, when available.
	Raw []byte

	// sentinel is the wrapped class error, exposed through Unwrap.
	sentinel error
}

// Error implements the error interface.
func (e *Error) Error() string {
	switch {
	case e.Type != "" && e.StatusCode != 0:
		return fmt.Sprintf("ai: %s error (%d %s): %s", e.Provider, e.StatusCode, e.Type, e.Message)
	case e.StatusCode != 0:
		return fmt.Sprintf("ai: %s error (%d): %s", e.Provider, e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("ai: %s error: %s", e.Provider, e.Message)
	}
}

// Unwrap returns the wrapped sentinel error so [errors.Is] matches ErrAuth,
// ErrRateLimited, and the other class errors.
func (e *Error) Unwrap() error {
	return e.sentinel
}

// WithSentinel returns a copy of e wrapping the given sentinel. Adapters use it
// when they can classify an error more precisely than [ClassifyStatus] does.
func (e *Error) WithSentinel(sentinel error) *Error {
	clone := *e
	clone.sentinel = sentinel

	return &clone
}

// NewError builds an [Error] for the given provider and HTTP status, selecting
// the wrapped sentinel from the status code via [ClassifyStatus].
func NewError(provider Provider, status int, message string) *Error {
	return &Error{
		Provider:   provider,
		StatusCode: status,
		Message:    message,
		sentinel:   ClassifyStatus(status),
	}
}

// ClassifyStatus maps an HTTP status code to the matching sentinel error, or
// nil for 2xx. Anthropic's 529 is treated as overload.
func ClassifyStatus(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ErrAuth
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == 529:
		return ErrOverloaded
	case status >= 500:
		return ErrOverloaded
	case status == http.StatusRequestTimeout:
		return ErrOverloaded
	case status >= 400:
		return ErrInvalidRequest
	default:
		return nil
	}
}

// IsRetryable reports whether err is worth retrying: rate limits, overload,
// request timeouts, and transport-level errors. Invalid-request and auth
// errors are not retryable. A nil error is not retryable.
//
// Retryability is orthogonal to whether a retry is safe mid-stream; the retry
// middleware only replays requests that have not yet produced output.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}

	var responseErr *ResponseError
	if errors.As(err, &responseErr) && (responseErr.FinishReason == FinishLength || responseErr.FinishReason == FinishContentFilter) {
		return false
	}

	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller's context is gone; retrying under it cannot succeed.
		return false
	case errors.Is(err, ErrContentFiltered), errors.Is(err, ErrRefused), errors.Is(err, ErrResponseTruncated):
		return false
	case errors.Is(err, ErrIncompleteStream), errors.Is(err, ErrEmptyResponse), errors.Is(err, ErrResponseFailed):
		return true
	case errors.Is(err, ErrStreamIdle), errors.Is(err, ErrResponseTimeout):
		// A stream that went silent, or a peer that never started answering, is
		// retryable even though the abort itself is local: the provider may
		// answer normally on a fresh attempt. The context arm above stays first
		// so a cancelled turn still loses.
		return true
	case isCertificateFailure(err):
		// A certificate the client cannot verify is not transient: the caller
		// has to fix it, so report it on the first attempt. Transient TLS
		// conditions (handshake timeouts, resets) stay retryable.
		return false
	case errors.Is(err, ErrRateLimited), errors.Is(err, ErrOverloaded):
		return true
	case errors.Is(err, ErrAuth), errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrUnsupported):
		return false
	}

	var apiErr *Error
	if errors.As(err, &apiErr) {
		// A structured error with no sentinel and a 2xx/0 status is a
		// transport or decode failure surfaced during the call: retry it.
		return apiErr.StatusCode == 0
	}
	// Bare (non-*Error) errors reaching the retry layer are transport errors
	// such as connection resets or timeouts.
	return true
}

// isCertificateFailure reports whether err is a certificate validation failure
// rather than a transient TLS condition.
func isCertificateFailure(err error) bool {
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return true
	}

	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalid          x509.CertificateInvalidError
	)

	return errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostname) ||
		errors.As(err, &invalid)
}

// NewRetryNotice builds the notice for the retry that is about to start after
// err. attempt is its 1-based ordinal and budget is the number of retries
// available. The reason is classified provider-neutrally, so the same failure
// renders the same way whoever re-issues the request: the retry middleware or
// an agent loop replaying a turn.
func NewRetryNotice(attempt, budget int, delay time.Duration, err error) *RetryNotice {
	if attempt < 1 {
		attempt = 1
	}

	if budget < attempt {
		budget = attempt
	}

	var response *ResponseError
	if errors.As(err, &response) {
		clone := *response
		response = &clone
	}

	return &RetryNotice{
		Response:   response,
		Attempt:    attempt,
		MaxRetries: budget,
		Delay:      delay,
		Reason:     retryReason(err),
	}
}

// retryReason renders the failure class a frontend shows next to the wait. It
// stays a short phrase: the provider's own wording belongs to the terminal
// error, which still carries the full chain.
func retryReason(err error) string {
	switch {
	case errors.Is(err, ErrIncompleteStream):
		return "incomplete stream"
	case errors.Is(err, ErrEmptyResponse):
		var responseErr *ResponseError
		if errors.As(err, &responseErr) && responseErr.HasReasoning {
			return "reasoning-only response"
		}

		return "empty response"
	case errors.Is(err, ErrResponseFailed):
		return "response failed"
	case errors.Is(err, ErrRateLimited):
		return "rate limited"
	case errors.Is(err, ErrOverloaded):
		return "provider overloaded"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "stream ended early"
	case errors.Is(err, ErrStreamIdle):
		return "stream idle"
	case errors.Is(err, ErrResponseTimeout):
		return "response timed out"
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	default:
		return "connection error"
	}
}
