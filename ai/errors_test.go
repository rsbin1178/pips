package ai_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		want   error
	}{
		{200, nil},
		{201, nil},
		{400, ai.ErrInvalidRequest},
		{401, ai.ErrAuth},
		{403, ai.ErrAuth},
		{404, ai.ErrInvalidRequest},
		{408, ai.ErrOverloaded},
		{422, ai.ErrInvalidRequest},
		{429, ai.ErrRateLimited},
		{500, ai.ErrOverloaded},
		{502, ai.ErrOverloaded},
		{503, ai.ErrOverloaded},
		{529, ai.ErrOverloaded},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ai.ClassifyStatus(tc.status))
		})
	}
}

func TestErrorSentinelsAndFormatting(t *testing.T) {
	t.Parallel()

	err := ai.NewError(ai.ProviderOpenAI, 429, "You exceeded your quota")
	require.ErrorIs(t, err, ai.ErrRateLimited)

	var apiErr *ai.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 429, apiErr.StatusCode)
	assert.Contains(t, err.Error(), "openai")
	assert.Contains(t, err.Error(), "429")

	// Wrapping keeps both errors.Is and errors.As working.
	wrapped := fmt.Errorf("generate: %w", err)
	require.ErrorIs(t, wrapped, ai.ErrRateLimited)
	require.ErrorAs(t, wrapped, &apiErr)
}

func TestErrorWithSentinel(t *testing.T) {
	t.Parallel()

	base := ai.NewError(ai.ProviderAnthropic, 400, "thinking not supported")
	require.ErrorIs(t, base, ai.ErrInvalidRequest)

	reclassified := base.WithSentinel(ai.ErrUnsupported)
	require.ErrorIs(t, reclassified, ai.ErrUnsupported)
	require.NotErrorIs(t, reclassified, ai.ErrInvalidRequest)
	// The original is untouched.
	require.ErrorIs(t, base, ai.ErrInvalidRequest)
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limited", ai.NewError(ai.ProviderOpenAI, 429, "slow down"), true},
		{"overloaded 529", ai.NewError(ai.ProviderAnthropic, 529, "overloaded"), true},
		{"server 500", ai.NewError(ai.ProviderGemini, 500, "internal"), true},
		{"server 503", ai.NewError(ai.ProviderOpenAI, 503, "unavailable"), true},
		{"bad request", ai.NewError(ai.ProviderOpenAI, 400, "bad schema"), false},
		{"auth", ai.NewError(ai.ProviderOpenAI, 401, "bad key"), false},
		{"not found", ai.NewError(ai.ProviderOpenAI, 404, "no such model"), false},
		{"unsupported", fmt.Errorf("images: %w", ai.ErrUnsupported), false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"wrapped cancel", fmt.Errorf("do: %w", context.Canceled), false},
		{"stream idle", ai.ErrStreamIdle, true},
		{"wrapped stream idle", fmt.Errorf("openai stream: %w", ai.ErrStreamIdle), true},
		{"response timed out", ai.ErrResponseTimeout, true},
		{"wrapped response timed out", fmt.Errorf("openai: responses stream: %w", ai.ErrResponseTimeout), true},
		{
			// A peer that never answered is worth another attempt; a turn the
			// caller cancelled is not, so the context arm still wins.
			"cancelled response timed out",
			fmt.Errorf("openai stream: %w", errors.Join(ai.ErrResponseTimeout, context.Canceled)),
			false,
		},
		{
			// A parked stream is retryable, but a cancelled turn is not: the
			// context arm must win even when the abort carries both.
			"cancelled stream idle",
			fmt.Errorf("openai stream: %w", errors.Join(ai.ErrStreamIdle, context.Canceled)),
			false,
		},
		{"net timeout", &net.OpError{Op: "dial", Err: timeoutError{}}, true},
		{
			"TLS handshake timeout",
			fmt.Errorf(
				"openai stream: %w",
				&url.Error{Op: "Post", URL: "https://example.invalid", Err: errors.New("net/http: TLS handshake timeout")},
			),
			true,
		},
		{"plain transport error", errors.New("connection reset by peer"), true},
		{"wrapped rate limit", fmt.Errorf("call: %w", ai.NewError(ai.ProviderOpenAI, 429, "x")), true},
		{
			// A certificate the client cannot verify is the user's to fix, so
			// it is reported on the first attempt instead of being replayed.
			"certificate verification",
			&url.Error{Op: "Post", URL: "https://example.invalid", Err: &tls.CertificateVerificationError{}},
			false,
		},
		{
			"unknown authority",
			fmt.Errorf("dial: %w", x509.UnknownAuthorityError{}),
			false,
		},
		{
			"certificate hostname mismatch",
			x509.HostnameError{Certificate: &x509.Certificate{}, Host: "example.invalid"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ai.IsRetryable(tc.err))
		})
	}
}

func TestErrorRetryAfterField(t *testing.T) {
	t.Parallel()

	err := ai.NewError(ai.ProviderOpenAI, 429, "slow down")
	err.RetryAfter = 3 * time.Second

	var apiErr *ai.Error
	require.ErrorAs(t, fmt.Errorf("wrap: %w", err), &apiErr)
	assert.Equal(t, 3*time.Second, apiErr.RetryAfter)
}
