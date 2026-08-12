package team

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangesParityPaginationAndDefensiveCopies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		store func(*testing.T) Store
	}{
		{name: "memory", store: func(t *testing.T) Store {
			t.Helper()

			store, err := NewMemoryStore()
			require.NoError(t, err)

			return store
		}},
		{name: "jsonl", store: func(t *testing.T) Store {
			t.Helper()

			store, err := NewJSONLStore(filepath.Join(t.TempDir(), "teams"))
			require.NoError(t, err)

			return store
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := test.store(t)
			runtime := newTestRuntime(t, store)
			group := registerWorker(t, runtime, createTestTeam(t, runtime))
			sent, err := runtime.engine.SendMessage(t.Context(), group.ID, SendMessageRequest{
				Command: runtime.member("lead", group.Revision), MessageID: "change-message",
				RecipientID: "worker", Body: ai.JSON(`{"text":"change"}`),
			})
			require.NoError(t, err)

			group = sent.Team

			first, err := runtime.engine.Changes(t.Context(), group.ID, ChangeOptions{Limit: 2})
			require.NoError(t, err)
			require.Len(t, first.Changes, 2)
			assert.Equal(t, Revision(2), first.NextAfter)
			second, err := runtime.engine.Changes(t.Context(), group.ID, ChangeOptions{
				AfterRevision: first.NextAfter, Limit: 2,
			})
			require.NoError(t, err)
			require.Len(t, second.Changes, 1)
			assert.Equal(t, CauseMessageSent, second.Changes[0].Transition.Cause)
			require.NotNil(t, second.Changes[0].Message)
			second.Changes[0].Message.Body[0] = '['

			again, err := runtime.engine.Changes(t.Context(), group.ID, ChangeOptions{
				AfterRevision: first.NextAfter, Limit: 2,
			})
			require.NoError(t, err)
			assert.JSONEq(t, `{"text":"change"}`, string(again.Changes[0].Message.Body))
			empty, err := runtime.engine.Changes(t.Context(), group.ID, ChangeOptions{
				AfterRevision: second.NextAfter, Limit: 2,
			})
			require.NoError(t, err)
			assert.Empty(t, empty.Changes)
			assert.Equal(t, second.NextAfter, empty.NextAfter)
		})
	}
}

type staleFirstLoadStore struct {
	Store
	stale Record
	once  sync.Once
}

func (store *staleFirstLoadStore) Load(ctx context.Context, id ID) (Record, error) {
	var record Record

	stale := false

	store.once.Do(func() {
		record = cloneRecord(store.stale)
		stale = true
	})

	if stale {
		return record, nil
	}

	return store.Store.Load(ctx, id)
}

func TestMailboxValidatesAgainstSnapshotAfterConcurrentDelivery(t *testing.T) {
	t.Parallel()

	memory, err := NewMemoryStore()
	require.NoError(t, err)
	runtime := newTestRuntime(t, memory)
	group := registerWorker(t, runtime, createTestTeam(t, runtime))
	stale, err := memory.Load(t.Context(), group.ID)
	require.NoError(t, err)

	sent, err := runtime.engine.SendMessage(t.Context(), group.ID, SendMessageRequest{
		Command: runtime.member("lead", group.Revision), MessageID: "concurrent-message",
		RecipientID: "worker", Body: ai.JSON(`{"text":"new"}`),
	})
	require.NoError(t, err)

	engine, err := New(&staleFirstLoadStore{Store: memory, stale: stale})
	require.NoError(t, err)
	page, err := engine.Mailbox(t.Context(), sent.Team.ID, "worker", MailboxOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	assert.Equal(t, MessageID("concurrent-message"), page.Messages[0].ID)
}

func TestValidateMailboxTransitionRejectsUnrelatedCursorChanges(t *testing.T) {
	t.Parallel()

	memory, err := NewMemoryStore()
	require.NoError(t, err)
	runtime := newTestRuntime(t, memory)
	group := registerWorker(t, runtime, createTestTeam(t, runtime))
	history, err := memory.History(t.Context(), group.ID)
	require.NoError(t, err)
	require.Len(t, history, 2)

	next := cloneRecord(history[1])
	next.Team.Members[0].MailboxDelivered = 1
	assert.Error(t, validateNextRecord(history[0], history[0].Team.Revision, next))
}

type changeOverrideStore struct {
	Store
	page ChangePage
}

func (store changeOverrideStore) Changes(context.Context, ID, ChangeOptions) (ChangePage, error) {
	return store.page, nil
}

func TestEngineRejectsMalformedCustomChangePage(t *testing.T) {
	t.Parallel()

	memory, err := NewMemoryStore()
	require.NoError(t, err)
	runtime := newTestRuntime(t, memory)
	group := createTestTeam(t, runtime)
	history, err := memory.History(t.Context(), group.ID)
	require.NoError(t, err)

	invalid := Change{Transition: history[0].Transition}
	invalid.Transition.Revision = 99
	engine, err := New(changeOverrideStore{Store: memory, page: ChangePage{
		Changes: []Change{invalid}, NextAfter: 99,
	}})
	require.NoError(t, err)

	_, err = engine.Changes(t.Context(), group.ID, ChangeOptions{Limit: 1})
	require.ErrorIs(t, err, ErrCorruptStore)

	invalid = Change{Transition: history[0].Transition}
	invalid.Transition.ID = "not safe"
	engine, err = New(changeOverrideStore{Store: memory, page: ChangePage{
		Changes: []Change{invalid}, NextAfter: 1,
	}})
	require.NoError(t, err)
	_, err = engine.Changes(t.Context(), group.ID, ChangeOptions{Limit: 1})
	assert.ErrorIs(t, err, ErrCorruptStore)
}

func TestV2MessageJournalStoresEachBodyOnceAtScale(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("scale test")
	}

	store, err := NewMemoryStore()
	require.NoError(t, err)
	runtime := newTestRuntime(t, store)
	group := registerWorker(t, runtime, createTestTeam(t, runtime))

	for index := range 2_000 {
		marker := fmt.Sprintf("payload-%04d", index)
		sent, sendErr := runtime.engine.SendMessage(t.Context(), group.ID, SendMessageRequest{
			Command:   runtime.member("lead", group.Revision),
			MessageID: MessageID(fmt.Sprintf("message-%04d", index)), RecipientID: "worker",
			Body: ai.JSON(fmt.Sprintf(`{"text":%q}`, marker)),
		})
		require.NoError(t, sendErr)

		group = sent.Team
	}

	records, err := store.History(t.Context(), group.ID)
	require.NoError(t, err)
	data, err := encodeV2File(records, store.config.limits)
	require.NoError(t, err)

	for _, marker := range []string{"payload-0000", "payload-0999", "payload-1999"} {
		assert.Equal(t, 1, bytes.Count(data, []byte(marker)))
	}

	firstMessageRecord, err := encodedRecord(records[2], store.config.limits)
	require.NoError(t, err)
	lastMessageRecord, err := encodedRecord(records[len(records)-1], store.config.limits)
	require.NoError(t, err)

	sizeDifference := len(lastMessageRecord) - len(firstMessageRecord)
	if sizeDifference < 0 {
		sizeDifference = -sizeDifference
	}

	assert.Less(t, sizeDifference, 256, "message record size must not grow with message history")

	mailbox, err := store.Mailbox(t.Context(), group.ID, "worker", MailboxOptions{
		AfterSequence: 1_990, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, mailbox.Messages, 10)
	assert.Equal(t, uint64(1_991), mailbox.Messages[0].Sequence)
	assert.Equal(t, uint64(2_000), mailbox.NextAfter)
}
