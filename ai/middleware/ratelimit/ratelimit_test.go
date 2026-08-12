package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/ratelimit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubModel struct {
	calls int
}

func (m *stubModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	m.calls++
	return &ai.Response{Message: ai.AssistantText("ok")}, nil
}

func (m *stubModel) Stream(context.Context, ai.Request) ai.Stream {
	m.calls++

	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}, nil)
	}
}

func (m *stubModel) Provider() ai.Provider         { return ai.ProviderOpenAI }
func (m *stubModel) ModelID() string               { return "test" }
func (m *stubModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true} }

// countingModel also implements ai.TokenCounter.
type countingModel struct {
	stubModel
	tokens int
}

func (m *countingModel) CountTokens(context.Context, ai.Request) (int, error) {
	return m.tokens, nil
}

func TestAllowsWithinBudget(t *testing.T) {
	t.Parallel()

	base := &stubModel{}
	model := ratelimit.New(ratelimit.WithRPM(600), ratelimit.WithTPM(60000))(base)

	// The first request draws from the initial burst and does not block.
	done := make(chan error, 1)

	go func() {
		_, err := model.Generate(context.Background(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
		assert.Equal(t, 1, base.calls)
	case <-time.After(2 * time.Second):
		t.Fatal("first request should not block within budget")
	}
}

func TestContextCancellationDuringWait(t *testing.T) {
	t.Parallel()

	base := &stubModel{}
	// RPM 60 → burst 60; drain it, then the next Wait blocks until refill.
	model := ratelimit.New(ratelimit.WithRPM(60))(base)

	ctx := context.Background()
	for range 60 {
		_, err := model.Generate(ctx, ai.Request{})
		require.NoError(t, err)
	}

	// The 61st waits ~1s for a token; a short deadline makes Wait give up.
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	_, err := model.Generate(cctx, ai.Request{})
	require.Error(t, err)
	assert.Equal(t, 60, base.calls, "the blocked request never reached the model")
}

func TestUsesTokenCounterForTPM(t *testing.T) {
	t.Parallel()

	// TPM burst 100; a request the counter reports as 100 tokens drains it in
	// one call, so the second must wait (and we cancel to observe the block).
	base := &countingModel{tokens: 100}
	model := ratelimit.New(ratelimit.WithTPM(100))(base)

	ctx := context.Background()
	_, err := model.Generate(ctx, ai.Request{Messages: []ai.Message{ai.UserText("x")}})
	require.NoError(t, err)

	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	_, err = model.Generate(cctx, ai.Request{Messages: []ai.Message{ai.UserText("x")}})
	require.Error(t, err)
	assert.Equal(t, 1, base.calls, "the token-starved request never reached the model")
}

func TestNoLimitersPassThrough(t *testing.T) {
	t.Parallel()

	base := &stubModel{}
	model := ratelimit.New()(base) // no RPM/TPM configured

	for range 100 {
		_, err := model.Generate(context.Background(), ai.Request{})
		require.NoError(t, err)
	}

	assert.Equal(t, 100, base.calls)
}
