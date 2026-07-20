package approval

import (
	"encoding/json"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeReceiptUsesStrictVersionedContract(t *testing.T) {
	t.Parallel()

	valid := receipt{
		Event:       eventRequested,
		RequestID:   "00000000000000000000000000000000",
		CallID:      "call-1",
		Tool:        "shell",
		Fingerprint: execution.Fingerprint{}.String(),
	}
	data, err := json.Marshal(valid)
	require.NoError(t, err)

	decoded, fingerprint, err := decodeReceipt(data)
	require.NoError(t, err)
	assert.Equal(t, valid, decoded)
	assert.Equal(t, execution.Fingerprint{}, fingerprint)

	invalid := []ai.JSON{
		ai.JSON(`{}`),
		append(data[:len(data)-1], []byte(`,"unknown":true}`)...),
		append(data, []byte(` {}`)...),
		ai.JSON(`{"event":"requested","request_id":"UPPER","call_id":"call-1","tool":"shell","fingerprint":"` +
			execution.Fingerprint{}.String() + `"}`),
		ai.JSON(`{"event":"started","request_id":"00000000000000000000000000000000","call_id":"call-1","tool":"shell","fingerprint":"` +
			execution.Fingerprint{}.String() + `","attempt":0}`),
	}

	for _, input := range invalid {
		_, _, err := decodeReceipt(input)
		require.Error(t, err)
	}
}

func TestReplayRejectsCompletionThatDisagreesWithToolResult(t *testing.T) {
	t.Parallel()

	session := newMemorySession()
	flow := appendStartedLifecycle(t, session)
	session.appendToolResult(agent.ToolCall{ID: flow.receipt.CallID, Name: flow.receipt.Tool}, false)
	require.NoError(t, appendReceipt(session, receiptFor(
		flow,
		eventCompleted,
		"",
		1,
		journalResultError,
	)))

	replay := replayJournal(session.Path())
	assert.True(t, replay.tainted)
}

func TestReplayRequiresDurableResultBeforeCompletion(t *testing.T) {
	t.Parallel()

	session := newMemorySession()
	flow := appendStartedLifecycle(t, session)
	require.NoError(t, appendReceipt(session, receiptFor(
		flow,
		eventCompleted,
		"",
		1,
		journalResultSuccess,
	)))

	replay := replayJournal(session.Path())
	assert.True(t, replay.tainted)
}

func TestReplayAcceptsOrphanAcknowledgementWithoutInventingResult(t *testing.T) {
	t.Parallel()

	session := newMemorySession()
	flow := appendStartedLifecycle(t, session)
	require.NoError(t, appendReceipt(session, receiptFor(
		flow,
		eventAcknowledged,
		ChoiceMarkFailed,
		1,
		"",
	)))

	replay := replayJournal(session.Path())
	assert.False(t, replay.tainted)
	require.Len(t, replay.lifecycles, 1)
	assert.True(t, replay.lifecycles[0].acknowledged)
}

func appendStartedLifecycle(t *testing.T, session *memorySession) *lifecycle {
	t.Helper()

	record := receipt{
		Event:       eventRequested,
		RequestID:   "00000000000000000000000000000000",
		CallID:      "call-1",
		Tool:        controlledToolName,
		Fingerprint: execution.Fingerprint{}.String(),
	}
	require.NoError(t, appendReceipt(session, record))

	replay := replayJournal(session.Path())
	flow := replay.byRequest[record.RequestID]
	require.NotNil(t, flow)
	require.NoError(t, appendReceipt(session, receiptFor(flow, eventStarted, "", 1, "")))

	return flow
}

func FuzzDecodeReceipt(f *testing.F) {
	f.Add([]byte(`{
		"event":"requested",
		"request_id":"00000000000000000000000000000000",
		"call_id":"call-1",
		"tool":"shell",
		"fingerprint":"0000000000000000000000000000000000000000000000000000000000000000"
	}`))
	f.Add([]byte(`not-json`))

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _, _ = decodeReceipt(data)
	})
}
