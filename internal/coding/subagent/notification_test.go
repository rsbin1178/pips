package subagent

import (
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotificationInboxPersistsIdempotentDelivery(t *testing.T) {
	t.Parallel()

	parent, err := harness.NewSession(harness.NewMemoryStore("s-parent"))
	require.NoError(t, err)
	inbox, err := NewNotificationInbox(parent, "s-parent", DefaultLimits().MaxResultBytes)
	require.NoError(t, err)

	terminalAt := time.Now().UTC()
	result := Result{
		Role: RoleExplore, ChildSessionID: "s-child", Outcome: OutcomeSucceeded,
		Code: "ok", Value: ExploreResult{Summary: "located", Evidence: []Evidence{}, Unknowns: []string{}},
		Usage: ai.Usage{InputTokens: 10, OutputTokens: 3},
	}
	event := Event{
		State: StateSucceeded, Role: RoleExplore, ChildSessionID: "s-child",
		ParentSessionID: "s-parent", ParentInteractionID: "interaction-1",
		ParentRunID: "run-1", ParentToolCallID: "call-1",
		RootInteractionID: "interaction-1", Delivery: DeliveryBackground,
		TaskPreview: "inspect runtime", Code: "ok", Time: terminalAt, Result: &result,
	}
	require.NoError(t, inbox.EnqueueCompletion(event))
	require.NoError(t, inbox.EnqueueCompletion(event))

	pending, err := inbox.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "s-child", pending[0].ID)
	assert.JSONEq(t, `{"summary":"located","evidence":[],"unknowns":[]}`, string(pending[0].Result))

	require.NoError(t, inbox.Acknowledge("s-child"))
	require.NoError(t, inbox.Acknowledge("s-child"))
	pending, err = inbox.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)

	reopened, err := NewNotificationInbox(parent, "s-parent", DefaultLimits().MaxResultBytes)
	require.NoError(t, err)
	known, err := reopened.Contains("s-child")
	require.NoError(t, err)
	assert.True(t, known)
}

func TestNotificationInboxRecoversInterruptedBackgroundChild(t *testing.T) {
	t.Parallel()

	parent, err := harness.NewSession(harness.NewMemoryStore("s-parent"))
	require.NoError(t, err)
	inbox, err := NewNotificationInbox(parent, "s-parent", DefaultLimits().MaxResultBytes)
	require.NoError(t, err)

	summary := Summary{
		ChildSessionID: "s-child",
		Ownership: Ownership{
			ParentSessionID: "s-parent", ParentInteractionID: "interaction-1",
			ParentRunID: "run-1", ParentToolCallID: "call-1",
			RootInteractionID: "interaction-1",
		},
		Delivery: DeliveryBackground, Role: RolePlan, State: StateInterrupted,
		TaskPreview: "plan work", CreatedAt: time.Now().UTC(), Code: "process_interrupted",
	}
	require.NoError(t, inbox.EnqueueRecovered(summary, nil))

	pending, err := inbox.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, OutcomeInterrupted, pending[0].Outcome)
	assert.Empty(t, pending[0].Result)
}
