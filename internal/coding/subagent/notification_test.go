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

func TestNotificationInboxCarriesParentMessagesOnTheSameQueue(t *testing.T) {
	t.Parallel()

	parent, err := harness.NewSession(harness.NewMemoryStore("s-parent"))
	require.NoError(t, err)
	inbox, err := NewNotificationInbox(parent, "s-parent", DefaultLimits().MaxResultBytes)
	require.NoError(t, err)

	require.NoError(t, inbox.Enqueue(testCompletion("s-child")))

	// One queue: a rendered parent message is a second record kind on the same
	// durable log, and it acknowledges through the same call.
	message := ParentMessage{
		ID:        ParentMessagePrefix + "1-1",
		Ownership: Ownership{ParentSessionID: "s-parent"},
		Kind:      MessageKindBackgroundEvents,
		TaskID:    "monitor-1",
		Body:      "<monitor-event task_id=\"monitor-1\">\n[deploy log] FAILED\n</monitor-event>",
		At:        time.Now().UTC(),
	}
	require.NoError(t, inbox.EnqueueMessage(message))
	require.NoError(t, inbox.EnqueueMessage(message))

	pending, err := inbox.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "s-child", pending[0].ID)

	messages, err := inbox.PendingMessages()
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, message, messages[0])

	// Queued records are immutable for their id.
	changed := message
	changed.Body = "different body"
	require.ErrorIs(t, inbox.EnqueueMessage(changed), ErrInvalid)

	// Acknowledging one kind leaves the other pending.
	require.NoError(t, inbox.Acknowledge(message.ID))
	require.NoError(t, inbox.Acknowledge(message.ID))

	messages, err = inbox.PendingMessages()
	require.NoError(t, err)
	assert.Empty(t, messages)

	pending, err = inbox.Pending()
	require.NoError(t, err)
	assert.Len(t, pending, 1)

	require.NoError(t, inbox.Acknowledge("s-child"))

	pending, err = inbox.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)

	// Both kinds stay acknowledged across a reopen, so a restart never wakes
	// the model with what the conversation already committed.
	reopened, err := NewNotificationInbox(parent, "s-parent", DefaultLimits().MaxResultBytes)
	require.NoError(t, err)
	known, err := reopened.Contains(message.ID)
	require.NoError(t, err)
	assert.True(t, known)
	require.NoError(t, reopened.EnqueueMessage(message))
	messages, err = reopened.PendingMessages()
	require.NoError(t, err)
	assert.Empty(t, messages)
}

func TestNotificationInboxRefusesUnboundedParentMessages(t *testing.T) {
	t.Parallel()

	parent, err := harness.NewSession(harness.NewMemoryStore("s-parent"))
	require.NoError(t, err)
	inbox, err := NewNotificationInbox(parent, "s-parent", DefaultLimits().MaxResultBytes)
	require.NoError(t, err)

	base := ParentMessage{
		ID:        ParentMessagePrefix + "2-1",
		Ownership: Ownership{ParentSessionID: "s-parent"},
		Kind:      MessageKindBackgroundCompletion,
		TaskID:    "monitor-2",
		Body:      "Monitor \"monitor-2\" ended: [monitor ended: exited (code 0)].",
		At:        time.Now().UTC(),
	}
	require.NoError(t, inbox.EnqueueMessage(base))

	wrongSession := base
	wrongSession.ID = ParentMessagePrefix + "2-2"
	wrongSession.Ownership = Ownership{ParentSessionID: "s-other"}
	require.ErrorIs(t, inbox.EnqueueMessage(wrongSession), ErrInvalid)

	wrongKind := base
	wrongKind.ID = ParentMessagePrefix + "2-3"
	wrongKind.Kind = ParentMessageKind("unknown")
	require.ErrorIs(t, inbox.EnqueueMessage(wrongKind), ErrInvalid)

	emptyBody := base
	emptyBody.ID = ParentMessagePrefix + "2-4"
	emptyBody.Body = ""
	require.ErrorIs(t, inbox.EnqueueMessage(emptyBody), ErrInvalid)

	// A completion id may not be reused for a parent message: the two id spaces
	// stay disjoint so an acknowledgement can tell them apart.
	conflict := base
	conflict.ID = "s-child"
	require.ErrorIs(t, inbox.EnqueueMessage(conflict), ErrInvalid)
}

func testCompletion(childID string) Notification {
	return Notification{
		ID: childID, AgentID: childID,
		Ownership: Ownership{
			ParentSessionID: "s-parent", ParentInteractionID: "interaction-1",
			ParentRunID: "run-1", ParentToolCallID: "call-1",
			RootInteractionID: "interaction-1",
		},
		Role: RoleExplore, Outcome: OutcomeSucceeded, Code: "ok",
		TaskPreview: "inspect runtime", Result: ai.JSON(`{"summary":"located","evidence":[],"unknowns":[]}`),
		Usage: ai.Usage{InputTokens: 10, OutputTokens: 3}, TerminalAt: time.Now().UTC(),
	}
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
