package agent

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionInterjectPrecedesQueuedSteering pins the ordering an interjection
// promises: it is injected before every message already queued to steer, and
// it keeps the same drain order under both queue modes.
func TestSessionInterjectPrecedesQueuedSteering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode QueueMode
		want []string
	}{
		{name: "drain all", mode: QueueDrainAll, want: []string{"urgent", "first", "second"}},
		{name: "drain one", mode: QueueDrainOne, want: []string{"urgent"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			session := NewSession()
			session.Steer(ai.UserText("first"))
			session.Steer(ai.UserText("second"))
			session.Interject(ai.UserText("urgent"))

			drained := session.drainSteering(test.mode)
			require.Len(t, drained, len(test.want))

			for index, want := range test.want {
				assert.Equal(t, want, sessionMessageText(t, drained[index]))
			}
		})
	}
}

// TestSessionInterjectLeavesQueuedSteeringIntact proves an interjection does
// not alias or reorder the queue it precedes.
func TestSessionInterjectLeavesQueuedSteeringIntact(t *testing.T) {
	t.Parallel()

	session := NewSession()
	session.Steer(ai.UserText("first"))
	session.Interject(ai.UserText("urgent"))
	session.Interject(ai.UserText("later"))

	drained := session.drainSteering(QueueDrainAll)
	require.Len(t, drained, 3)
	assert.Equal(t, "later", sessionMessageText(t, drained[0]))
	assert.Equal(t, "urgent", sessionMessageText(t, drained[1]))
	assert.Equal(t, "first", sessionMessageText(t, drained[2]))

	session.Interject()
	assert.False(t, session.HasQueued())
}

func sessionMessageText(t *testing.T, message ai.Message) string {
	t.Helper()

	parts, err := ai.MessageParts(message)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	return text.Text
}
