package coding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reasoningOnlyModel streams one reasoning fragment and then fails on every
// call, so a test can read the recovery policy's budget without any answer
// content ever being produced.
type reasoningOnlyModel struct {
	calls atomic.Int32
}

func (m *reasoningOnlyModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("reasoningOnlyModel: Generate is not scripted")
}

func (m *reasoningOnlyModel) Stream(_ context.Context, _ ai.Request) ai.Stream {
	m.calls.Add(1)

	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{Type: ai.StreamMessageStart, ID: "resp"}, nil) {
			return
		}

		if !yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: "thought"}, nil) {
			return
		}

		yield(ai.StreamEvent{}, io.ErrUnexpectedEOF)
	}
}

func (m *reasoningOnlyModel) Provider() ai.Provider { return ai.Provider("reasoning-only") }
func (m *reasoningOnlyModel) ModelID() string       { return "reasoning-only-1" }
func (m *reasoningOnlyModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true}
}

// TestStreamRecoveryOptionGrantsTheModelReplayBudget drives the real Coding
// policy rather than restating its numbers, because the point of the split
// budget is that one value feeds both owners: a stream that failed after
// producing only reasoning earns the model middleware's own retry budget, so
// the two cannot drift apart. The consumer stops at the first notice, which
// abandons the run before the policy's two-second backoff, so the test reads the
// budget without waiting it out.
func TestStreamRecoveryOptionGrantsTheModelReplayBudget(t *testing.T) {
	t.Parallel()

	for _, continuation := range []bool{false, true} {
		t.Run(fmt.Sprintf("continuation=%t", continuation), func(t *testing.T) {
			t.Parallel()

			scripted := &reasoningOnlyModel{}

			a, err := agent.New(scripted, streamRecoveryOption(continuation))
			require.NoError(t, err)

			var (
				notice   *ai.RetryNotice
				discards int
			)

			for ev, streamErr := range a.Stream(t.Context(), agent.NewSession(), ai.UserText("hi")) {
				require.NoError(t, streamErr)

				if _, ok := ev.Payload().(agent.CandidateDiscarded); ok {
					discards++
				}

				payload, ok := ev.Payload().(agent.ModelStreamEvent)
				if !ok || payload.Event.Retry == nil {
					continue
				}

				notice = payload.Event.Retry

				break
			}

			require.NotNil(t, notice)
			assert.Equal(t, model.RetryBudget, notice.MaxRetries,
				"a reasoning-only failure earns the middleware's replay budget")
			assert.Equal(t, 1, notice.Attempt)
			assert.Equal(t, 1, discards, "the replay retracts the thinking the consumer rendered")
			assert.Equal(t, int32(1), scripted.calls.Load(),
				"the consumer stopped before the re-issue started")
		})
	}
}

var _ ai.LanguageModel = (*reasoningOnlyModel)(nil)
