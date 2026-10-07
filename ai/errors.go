package ai

import (
	"context"
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
)

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

	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller's context is gone; retrying under it cannot succeed.
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

// NewRetryNotice builds the notice for an attempt that is about to start after
// err. The reason is classified provider-neutrally, so the same failure renders
// the same way whoever re-issues the request: the retry middleware or an agent
// loop replaying a turn.
func NewRetryNotice(attempt, maxAttempts int, delay time.Duration, err error) *RetryNotice {
	if attempt < 1 {
		attempt = 1
	}

	if maxAttempts < attempt {
		maxAttempts = attempt
	}

	return &RetryNotice{
		Attempt: attempt,
		Max:     maxAttempts,
		Delay:   delay,
		Reason:  retryReason(err),
	}
}

// retryReason renders the failure class a frontend shows next to the wait. It
// stays a short phrase: the provider's own wording belongs to the terminal
// error, which still carries the full chain.
func retryReason(err error) string {
	switch {
	case errors.Is(err, ErrRateLimited):
		return "rate limited"
	case errors.Is(err, ErrOverloaded):
		return "provider overloaded"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "stream ended early"
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	default:
		return "connection error"
	}
}
