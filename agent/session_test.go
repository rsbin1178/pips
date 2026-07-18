package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionPendingScan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		msgs    []ai.Message
		wantIDs []string
	}{
		{
			name: "no messages",
		},
		{
			name: "text tail",
			msgs: []ai.Message{ai.UserText("hi"), ai.AssistantText("hello")},
		},
		{
			name: "unanswered calls",
			msgs: []ai.Message{
				ai.UserText("hi"),
				ai.Assistant(
					ai.ToolCallPart{ID: "c1", Name: "a"},
					ai.ToolCallPart{ID: "c2", Name: "b"},
				),
			},
			wantIDs: []string{"c1", "c2"},
		},
		{
			name: "partially answered",
			msgs: []ai.Message{
				ai.Assistant(
					ai.ToolCallPart{ID: "c1", Name: "a"},
					ai.ToolCallPart{ID: "c2", Name: "b"},
				),
				ai.ToolResultText("c1", "a", "done"),
			},
			wantIDs: []string{"c2"},
		},
		{
			name: "fully answered",
			msgs: []ai.Message{
				ai.Assistant(ai.ToolCallPart{ID: "c1", Name: "a"}),
				ai.ToolResultText("c1", "a", "done"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pending := agent.NewSession(tt.msgs...).Pending()

			ids := make([]string, 0, len(pending))
			for _, c := range pending {
				ids = append(ids, c.ID)
			}

			if tt.wantIDs == nil {
				assert.Empty(t, ids)
			} else {
				assert.Equal(t, tt.wantIDs, ids)
			}
		})
	}
}

func TestSessionResolvePending(t *testing.T) {
	t.Parallel()

	sess := agent.NewSession(
		ai.Assistant(
			ai.ToolCallPart{ID: "c1", Name: "a"},
			ai.ToolCallPart{ID: "c2", Name: "b"},
		),
	)

	err := sess.ResolvePending(t.Context(), func(_ context.Context, call ai.ToolCallPart) ([]ai.Part, error) {
		if call.ID == "c1" {
			return agent.TextResult("ok"), nil
		}

		return nil, errors.New("rejected")
	})
	require.NoError(t, err)
	assert.Empty(t, sess.Pending())

	msgs := sess.Messages()
	require.Len(t, msgs, 2)
	require.Equal(t, ai.RoleTool, msgs[1].Role)
	require.Len(t, msgs[1].Parts, 2)

	first, ok := msgs[1].Parts[0].(ai.ToolResultPart)
	require.True(t, ok)
	assert.False(t, first.IsError)

	second, ok := msgs[1].Parts[1].(ai.ToolResultPart)
	require.True(t, ok)
	assert.True(t, second.IsError)
	assert.Equal(t, []ai.Part{ai.Text("rejected")}, second.Content)

	// Resolving with nothing pending is a no-op.
	require.NoError(t, sess.ResolvePending(t.Context(), nil))
	assert.Len(t, sess.Messages(), 2)
}

func TestSessionResolveToolCallsSubsetInPendingOrder(t *testing.T) {
	t.Parallel()

	sess := agent.NewSession(ai.Assistant(
		ai.ToolCallPart{ID: "c1", Name: "first"},
		ai.ToolCallPart{ID: "c2", Name: "second"},
		ai.ToolCallPart{ID: "c3", Name: "third"},
	))

	err := sess.ResolveToolCalls(
		agent.ToolResolution{ToolCallID: "c3", Content: agent.TextResult("three")},
		agent.ToolResolution{ToolCallID: "c1", Content: agent.TextResult("one")},
	)
	require.NoError(t, err)

	pending := sess.Pending()
	require.Len(t, pending, 1)
	assert.Equal(t, "c2", pending[0].ID)

	msgs := sess.Messages()
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].Parts, 2)

	first, ok := msgs[1].Parts[0].(ai.ToolResultPart)
	require.True(t, ok)

	third, ok := msgs[1].Parts[1].(ai.ToolResultPart)
	require.True(t, ok)

	assert.Equal(t, "c1", first.ToolCallID)
	assert.Equal(t, "first", first.Name)
	assert.Equal(t, "c3", third.ToolCallID)

	data, err := json.Marshal(sess)
	require.NoError(t, err)

	restored := agent.NewSession()
	require.NoError(t, json.Unmarshal(data, restored))
	require.Len(t, restored.Pending(), 1)
	assert.Equal(t, "c2", restored.Pending()[0].ID)

	require.NoError(t, restored.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: "c2", Content: agent.TextResult("two"), IsError: true,
	}))
	assert.Empty(t, restored.Pending())
}

func TestSessionResolveToolCallsValidationIsAtomic(t *testing.T) {
	t.Parallel()

	newSession := func() *agent.Session {
		return agent.NewSession(ai.Assistant(
			ai.ToolCallPart{ID: "c1", Name: "first"},
			ai.ToolCallPart{ID: "c2", Name: "second"},
		))
	}

	tests := []struct {
		name        string
		resolutions []agent.ToolResolution
		want        error
	}{
		{
			name:        "empty ID",
			resolutions: []agent.ToolResolution{{Content: agent.TextResult("bad")}},
			want:        agent.ErrInvalidToolResolution,
		},
		{
			name: "duplicate ID",
			resolutions: []agent.ToolResolution{
				{ToolCallID: "c1"},
				{ToolCallID: "c1"},
			},
			want: agent.ErrInvalidToolResolution,
		},
		{
			name:        "stale ID",
			resolutions: []agent.ToolResolution{{ToolCallID: "missing"}},
			want:        agent.ErrToolCallNotPending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sess := newSession()
			before := sess.Messages()
			err := sess.ResolveToolCalls(tt.resolutions...)
			require.ErrorIs(t, err, tt.want)
			assert.Equal(t, before, sess.Messages())
			assert.Len(t, sess.Pending(), 2)
		})
	}
}

func TestSessionJSONRoundTrip(t *testing.T) {
	t.Parallel()

	sess := agent.NewSession(
		ai.UserText("hi"),
		ai.Assistant(
			ai.Text("thinking"),
			ai.ToolCallPart{ID: "c1", Name: "add", Args: ai.JSON(`{"a":1}`)},
		),
		ai.ToolResultText("c1", "add", "2"),
	)

	blob, err := json.Marshal(sess)
	require.NoError(t, err)

	restored := agent.NewSession()
	require.NoError(t, json.Unmarshal(blob, restored))

	assert.Equal(t, sess.Messages(), restored.Messages())
	assert.Equal(t, sess.Usage(), restored.Usage())
}

func TestSessionMessagesIsACopy(t *testing.T) {
	t.Parallel()

	sess := agent.NewSession(ai.UserText("hi"))

	msgs := sess.Messages()
	msgs[0] = ai.UserText("mutated")

	kept, ok := sess.Messages()[0].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "hi", kept.Text)
}

func TestSessionConcurrentAccess(t *testing.T) {
	t.Parallel()

	sess := agent.NewSession()

	var wg sync.WaitGroup

	for range 8 {
		wg.Add(2)

		go func() {
			defer wg.Done()

			sess.Append(ai.UserText("x"))
		}()

		go func() {
			defer wg.Done()

			_ = sess.Messages()
			_ = sess.Pending()
			_ = sess.Usage()
		}()
	}

	wg.Wait()
	assert.Len(t, sess.Messages(), 8)
}
