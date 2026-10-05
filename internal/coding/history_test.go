//nolint:wsl_v5 // Paging cases and their boundary assertions stay adjacent.
package coding

import (
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// historyPath builds a durable path of conversation messages and one interleaved
// non-message entry, so paging cannot assume every path entry is a message.
func historyPath(count int) []harness.Entry {
	path := make([]harness.Entry, 0, count+1)
	path = append(path, harness.Entry{
		Kind: harness.KindModelChange, ID: "model-1",
		Provider: ai.ProviderOpenAI, ModelID: "test-model",
	})
	for index := range count {
		path = append(path, harness.Entry{
			Kind:    harness.KindMessage,
			ID:      fmt.Sprintf("entry-%05d", index),
			Message: ai.UserText(fmt.Sprintf("M-%04d", index)),
		})
	}

	return path
}

// TestHistoryPageWalksBackwardsFromTheLoadedWindow is the AC8 arithmetic: the
// bootstrap window is the newest transcript cap, and history pages fill in the
// older messages contiguously.
func TestHistoryPageWalksBackwardsFromTheLoadedWindow(t *testing.T) {
	t.Parallel()

	const total = 5000
	path := historyPath(total)
	bootstrap, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Provider: ai.ProviderOpenAI, ModelID: "test-model",
		PlanMode: planmode.StateInactive, Path: path,
	})
	require.NoError(t, err)
	require.Len(t, bootstrap.State.Transcript, maxEventItems)
	assert.True(t, TranscriptWindowMayBeTruncated(bootstrap.State))
	loaded := len(bootstrap.State.Transcript)

	first := historyPage(path, loaded, HistoryRequest{Before: -1})
	assert.Equal(t, total, first.Total)
	assert.Equal(t, maxEventItems, loaded)
	assert.Equal(t, total-loaded-HistoryPageDefaultLimit, first.Start)
	require.Len(t, first.Messages, HistoryPageDefaultLimit)
	assert.Equal(t, ai.UserText("M-0804"), first.Messages[0])
	assert.Equal(t, ai.UserText("M-0903"), first.Messages[len(first.Messages)-1])
	assert.True(t, first.More, "older messages remain")

	second := historyPage(path, loaded, HistoryRequest{Before: first.Start})
	assert.Equal(t, first.Start-HistoryPageDefaultLimit, second.Start)
	require.Len(t, second.Messages, HistoryPageDefaultLimit)
	assert.Equal(t, ai.UserText("M-0704"), second.Messages[0])

	// The oldest page stops at zero and stops advertising more.
	oldest := historyPage(path, loaded, HistoryRequest{Before: 50})
	assert.Equal(t, 0, oldest.Start)
	assert.Len(t, oldest.Messages, 50)
	assert.Equal(t, ai.UserText("M-0000"), oldest.Messages[0])
	assert.False(t, oldest.More)

	// Zero before the oldest message yields nothing rather than an error: this is
	// how a caller asks for the boundaries alone.
	empty := historyPage(path, loaded, HistoryRequest{Before: 0})
	assert.Empty(t, empty.Messages)
	assert.Equal(t, 0, empty.Start)
	assert.Equal(t, total, empty.Total)
	assert.False(t, empty.More)

	// Before the end of the session is a legitimate read of the newest messages,
	// not an error.
	newest := historyPage(path, loaded, HistoryRequest{Before: total, Limit: 10})
	assert.Equal(t, total-10, newest.Start)
	assert.Equal(t, ai.UserText("M-4990"), newest.Messages[0])

	// Limit is clamped, and a short window is not truncated.
	clamped := historyPage(path, loaded, HistoryRequest{Before: 400, Limit: 10_000})
	assert.Len(t, clamped.Messages, HistoryPageMaxLimit)
	assert.Equal(t, 400-HistoryPageMaxLimit, clamped.Start)
}

// TestTranscriptWindowMayBeTruncatedOnlyAtTheCap keeps the frontend hint honest.
func TestTranscriptWindowMayBeTruncatedOnlyAtTheCap(t *testing.T) {
	t.Parallel()

	assert.False(t, TranscriptWindowMayBeTruncated(State{}))
	assert.False(t, TranscriptWindowMayBeTruncated(State{
		Transcript: make([]ai.Message, maxEventItems-1),
	}))
	assert.True(t, TranscriptWindowMayBeTruncated(State{
		Transcript: make([]ai.Message, maxEventItems),
	}))
}

// TestRuntimeHistoryReadsOlderDurableMessages covers the Runtime wrapper against a
// real session: appending to the durable path must not change the live State, and
// the read must stay bounded.
func TestRuntimeHistoryReadsOlderDurableMessages(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	baseline, err := runtime.History(t.Context(), HistoryRequest{Before: 0})
	require.NoError(t, err)

	for index := range 12 {
		_, appendErr := runtime.session.AppendMessage(ai.UserText(fmt.Sprintf("M-%02d", index)), nil)
		require.NoError(t, appendErr)
	}

	loaded := len(runtime.state.Transcript)

	page, err := runtime.History(t.Context(), HistoryRequest{Before: baseline.Total + 12, Limit: 4})
	require.NoError(t, err)
	assert.Equal(t, baseline.Total+12, page.Total)
	assert.Equal(t, baseline.Total+8, page.Start)
	assert.True(t, page.More)
	require.Len(t, page.Messages, 4)
	assert.Equal(t, ai.UserText("M-08"), page.Messages[0])
	assert.Equal(t, ai.UserText("M-11"), page.Messages[3])
	assert.Len(t, runtime.state.Transcript, loaded, "the read does not touch the live State")

	// A negative cursor means "before the bootstrap window", which for this
	// session is empty, so the newest page comes back.
	newest, err := runtime.History(t.Context(), HistoryRequest{Before: -1, Limit: 4})
	require.NoError(t, err)
	assert.Equal(t, baseline.Total+12-4, newest.Start)
	assert.Equal(t, ai.UserText("M-08"), newest.Messages[0])
	assert.True(t, newest.More)
}
