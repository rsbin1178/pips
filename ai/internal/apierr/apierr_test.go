package apierr_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/apierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeParsesEnvelopeAndPreservesMetadata(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":{"message":"Slow down","type":"rate_limit_error","code":"slow_down"}}`)

	err := apierr.Decode(ai.ProviderAgnes, http.StatusTooManyRequests, 12*time.Second, body)
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrRateLimited)

	var apiErr *ai.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, ai.ProviderAgnes, apiErr.Provider)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
	assert.Equal(t, "Slow down", apiErr.Message)
	assert.Equal(t, "rate_limit_error", apiErr.Type)
	assert.Equal(t, "slow_down", apiErr.Code)
	assert.Equal(t, 12*time.Second, apiErr.RetryAfter)
	assert.Equal(t, body, apiErr.Raw)
}

func TestDecodeEnvelopeVariants(t *testing.T) {
	t.Parallel()

	t.Run("numeric code is not a portable code", func(t *testing.T) {
		t.Parallel()

		err := apierr.Decode(ai.ProviderOpenAI, http.StatusBadRequest, 0,
			[]byte(`{"error":{"message":"Bad size","type":"invalid_request_error","code":123}}`))
		require.Error(t, err)

		var apiErr *ai.Error
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, "Bad size", apiErr.Message)
		assert.Equal(t, "invalid_request_error", apiErr.Type)
		assert.Empty(t, apiErr.Code)
	})

	t.Run("missing error object falls back to the raw body", func(t *testing.T) {
		t.Parallel()

		body := []byte(`{"detail":"not found"}`)

		err := apierr.Decode(ai.ProviderOpenAI, http.StatusNotFound, 0, body)
		require.Error(t, err)

		var apiErr *ai.Error
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, string(body), apiErr.Message)
		assert.Empty(t, apiErr.Type)
	})

	t.Run("non-JSON body is preserved verbatim", func(t *testing.T) {
		t.Parallel()

		body := []byte("<html>bad gateway</html>")

		err := apierr.Decode(ai.ProviderOpenAI, http.StatusBadGateway, 0, body)
		require.Error(t, err)

		var apiErr *ai.Error
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, string(body), apiErr.Message)
		assert.Equal(t, body, apiErr.Raw)
	})
}

func TestDecodeClassifiesStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status   int
		sentinel error
	}{
		{status: http.StatusUnauthorized, sentinel: ai.ErrAuth},
		{status: http.StatusForbidden, sentinel: ai.ErrAuth},
		{status: http.StatusPaymentRequired, sentinel: ai.ErrInvalidRequest},
		{status: http.StatusTooManyRequests, sentinel: ai.ErrRateLimited},
		{status: http.StatusServiceUnavailable, sentinel: ai.ErrOverloaded},
	}

	for _, tc := range cases {
		err := apierr.Decode(ai.ProviderAgnes, tc.status, 0, []byte(`{"error":{"message":"nope"}}`))
		require.ErrorIs(t, err, tc.sentinel, "status %d", tc.status)
	}
}
