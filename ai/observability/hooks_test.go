package observability_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingModel struct {
	resp    *ai.Response
	err     error
	events  []ai.StreamEvent
	lastCtx context.Context //nolint:containedctx // captured only to assert context propagation
}

func (m *recordingModel) Generate(ctx context.Context, _ ai.Request) (*ai.Response, error) {
	m.lastCtx = ctx
	return m.resp, m.err
}

func (m *recordingModel) Stream(ctx context.Context, _ ai.Request) ai.Stream {
	m.lastCtx = ctx

	return func(yield func(ai.StreamEvent, error) bool) {
		if m.err != nil {
			yield(ai.StreamEvent{}, m.err)
			return
		}

		for _, ev := range m.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (m *recordingModel) Provider() ai.Provider         { return ai.ProviderAnthropic }
func (m *recordingModel) ModelID() string               { return "claude-x" }
func (m *recordingModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true} }

func TestGenerateHooks(t *testing.T) {
	t.Parallel()

	base := &recordingModel{resp: &ai.Response{
		Message:      ai.AssistantText("hi"),
		FinishReason: ai.FinishStop,
		Usage:        ai.Usage{InputTokens: 5, OutputTokens: 2},
	}}

	var (
		started, finished bool
		gotResult         observability.Result
	)

	model := observability.Middleware(observability.Hooks{
		OnStart: func(ctx context.Context, info observability.CallInfo) context.Context {
			started = true

			assert.Equal(t, ai.ProviderAnthropic, info.Provider)
			assert.False(t, info.Streaming)

			return context.WithValue(ctx, ctxKey{}, "traced")
		},
		OnFinish: func(_ context.Context, result observability.Result) {
			finished = true
			gotResult = result
		},
	})(base)

	_, err := model.Generate(t.Context(), ai.Request{})
	require.NoError(t, err)

	assert.True(t, started)
	assert.True(t, finished)
	assert.Equal(t, ai.FinishStop, gotResult.FinishReason)
	assert.Equal(t, 5, gotResult.Usage.InputTokens)
	// OnStart's context reached the underlying model.
	assert.Equal(t, "traced", base.lastCtx.Value(ctxKey{}))
}

type ctxKey struct{}

func TestGenerateErrorHook(t *testing.T) {
	t.Parallel()

	base := &recordingModel{err: errors.New("boom")}

	var gotErr error

	model := observability.Middleware(observability.Hooks{
		OnError: func(_ context.Context, _ observability.CallInfo, err error) { gotErr = err },
	})(base)

	_, err := model.Generate(t.Context(), ai.Request{})
	require.Error(t, err)
	assert.Equal(t, err, gotErr)
}

func TestStreamHooks(t *testing.T) {
	t.Parallel()

	base := &recordingModel{events: []ai.StreamEvent{
		{Type: ai.StreamTextDelta, Text: "hi"},
		{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop, Usage: &ai.Usage{OutputTokens: 3}},
	}}

	var (
		eventCount int
		finish     observability.Result
	)

	model := observability.Middleware(observability.Hooks{
		OnStreamEvent: func(_ context.Context, _ observability.CallInfo, _ ai.StreamEvent) { eventCount++ },
		OnFinish:      func(_ context.Context, r observability.Result) { finish = r },
	})(base)

	_, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))
	require.NoError(t, err)

	assert.Equal(t, 2, eventCount)
	assert.Equal(t, ai.FinishStop, finish.FinishReason)
	assert.Equal(t, 3, finish.Usage.OutputTokens)
	assert.True(t, finish.Streaming)
}

func TestStreamErrorHook(t *testing.T) {
	t.Parallel()

	base := &recordingModel{err: errors.New("stream boom")}

	var gotErr error

	model := observability.Middleware(observability.Hooks{
		OnError: func(_ context.Context, _ observability.CallInfo, err error) { gotErr = err },
	})(base)

	for _, err := range model.Stream(t.Context(), ai.Request{}) {
		if err != nil {
			break
		}
	}

	require.Error(t, gotErr)
}
