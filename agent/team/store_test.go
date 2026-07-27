package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONLStoreReopensAndHandlesCommitMarkers(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "teams")
	store, err := NewJSONLStore(dir)
	require.NoError(t, err)
	runtime := newTestRuntime(t, store)
	team := createTestTeam(t, runtime)
	team = registerWorker(t, runtime, team)

	reopened, err := NewJSONLStore(dir)
	require.NoError(t, err)
	record, err := reopened.Load(t.Context(), team.ID)
	require.NoError(t, err)
	assert.Equal(t, team, record.Team)

	commandRecord, err := reopened.LoadCommand(t.Context(), team.ID, record.Transition.CommandID)
	require.NoError(t, err)
	assert.Equal(t, record, commandRecord)

	path := filepath.Join(dir, string(team.ID)+teamFileExt)
	directoryInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), directoryInfo.Mode().Perm())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // Test path is confined to t.TempDir.
	require.NoError(t, err)
	_, err = file.WriteString(`{"partial"`)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	record, err = reopened.Load(t.Context(), team.ID)
	require.NoError(t, err)
	assert.Equal(t, team, record.Team)

	recoveryEngine, err := New(reopened)
	require.NoError(t, err)
	team, err = recoveryEngine.RegisterMember(t.Context(), team.ID, RegisterMemberRequest{
		Command: CommandMetadata{
			ID: "after-torn-tail", ExpectedRevision: team.Revision,
			Actor: Actor{Kind: ActorKindCoordinator, ID: "recovery-coordinator"},
		},
		Member: MemberSpec{
			ID: "after-tail", Name: "After Tail", Role: "verify recovery",
		},
	})
	require.NoError(t, err)
	record, err = reopened.Load(t.Context(), team.ID)
	require.NoError(t, err)
	assert.Equal(t, team, record.Team)

	file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // Test path is confined to t.TempDir.
	require.NoError(t, err)
	_, err = file.WriteString(`{}`)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	_, err = reopened.Load(t.Context(), team.ID)
	require.ErrorIs(t, err, ErrCorruptStore)
}

func TestJSONLStoreRejectsNonPrivateJournal(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "teams")
	store, err := NewJSONLStore(dir)
	require.NoError(t, err)
	runtime := newTestRuntime(t, store)
	group := createTestTeam(t, runtime)
	path := filepath.Join(dir, string(group.ID)+teamFileExt)
	require.NoError(t, os.Chmod(path, 0o640)) //nolint:gosec // Deliberately verifies rejection of a group-readable journal.

	_, err = store.Load(t.Context(), group.ID)
	require.ErrorIs(t, err, ErrCorruptStore)
}

func TestMemoryStoreCASAllowsOneConcurrentCommand(t *testing.T) {
	t.Parallel()

	store, err := NewMemoryStore()
	require.NoError(t, err)
	runtime := newTestRuntime(t, store)
	team := createTestTeam(t, runtime)

	requests := []RegisterMemberRequest{
		{
			Command: runtime.coordinator(team.Revision),
			Member:  MemberSpec{ID: "one", Name: "One", Role: "worker"},
		},
		{
			Command: runtime.coordinator(team.Revision),
			Member:  MemberSpec{ID: "two", Name: "Two", Role: "worker"},
		},
	}

	errorsByCommand := make([]error, len(requests))

	var wait sync.WaitGroup
	wait.Add(len(requests))

	for index := range requests {
		go func() {
			defer wait.Done()

			_, errorsByCommand[index] = runtime.engine.RegisterMember(
				t.Context(), team.ID, requests[index],
			)
		}()
	}

	wait.Wait()

	successes := 0
	conflicts := 0

	for _, err := range errorsByCommand {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			require.NoError(t, err)
		}
	}

	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
}

func TestStoreListIsLexicographicAndDefensive(t *testing.T) {
	t.Parallel()

	store, err := NewMemoryStore()
	require.NoError(t, err)
	runtime := newTestRuntime(t, store)

	for _, id := range []ID{"team-c", "team-a", "team-b"} {
		_, err = runtime.engine.Create(t.Context(), CreateRequest{
			Command: runtime.coordinator(0), ID: id, Objective: "objective",
			Lead: MemberSpec{ID: "lead", Name: "Lead", Role: "lead"},
		})
		require.NoError(t, err)
	}

	page, err := store.List(t.Context(), ListOptions{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.Teams, 2)
	assert.Equal(t, ID("team-a"), page.Teams[0].ID)
	assert.Equal(t, ID("team-b"), page.Teams[1].ID)
	assert.Equal(t, "team-b", page.NextCursor)

	page.Teams[0].Members[0].Name = "mutated"
	loaded, err := store.Load(t.Context(), "team-a")
	require.NoError(t, err)
	assert.Equal(t, "Lead", loaded.Team.Members[0].Name)
}

type loadOverrideStore struct {
	Store
	record Record
}

func (store loadOverrideStore) Load(context.Context, ID) (Record, error) {
	return store.record, nil
}

func TestEngineRejectsInvalidCustomStoreRecords(t *testing.T) {
	t.Parallel()

	memory, err := NewMemoryStore()
	require.NoError(t, err)
	runtime := newTestRuntime(t, memory)
	team := createTestTeam(t, runtime)
	record, err := memory.Load(t.Context(), team.ID)
	require.NoError(t, err)

	record.Team.Revision = 0
	engine, err := New(loadOverrideStore{Store: memory, record: record})
	require.NoError(t, err)

	_, err = engine.Get(t.Context(), team.ID)
	require.ErrorIs(t, err, ErrCorruptStore)
	_, err = engine.RegisterMember(t.Context(), team.ID, RegisterMemberRequest{
		Command: CommandMetadata{
			ID: "command", ExpectedRevision: team.Revision,
			Actor: Actor{Kind: ActorKindCoordinator, ID: "coordinator"},
		},
		Member: MemberSpec{ID: "worker", Name: "Worker", Role: "work"},
	})
	require.ErrorIs(t, err, ErrCorruptStore)
}

func TestStoreRejectsDuplicateEventID(t *testing.T) {
	t.Parallel()

	store, err := NewMemoryStore()
	require.NoError(t, err)
	engine, err := New(
		store,
		WithClock(ClockFunc(func() time.Time { return time.Now().UTC() })),
		WithEventIDSource(func(time.Time) (EventID, error) { return "same-event", nil }),
	)
	require.NoError(t, err)

	team, err := engine.Create(t.Context(), CreateRequest{
		Command: CommandMetadata{
			ID: "create", Actor: Actor{Kind: ActorKindCoordinator, ID: "coordinator"},
		},
		ID: "team-events", Objective: "objective",
		Lead: MemberSpec{ID: "lead", Name: "Lead", Role: "lead"},
	})
	require.NoError(t, err)

	_, err = engine.RegisterMember(t.Context(), team.ID, RegisterMemberRequest{
		Command: CommandMetadata{
			ID: "register", ExpectedRevision: team.Revision,
			Actor: Actor{Kind: ActorKindCoordinator, ID: "coordinator"},
		},
		Member: MemberSpec{ID: "worker", Name: "Worker", Role: "work"},
	})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestMailboxPersistsAcrossJSONLReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	require.NoError(t, err)
	runtime := newTestRuntime(t, store)
	team := registerWorker(t, runtime, createTestTeam(t, runtime))
	sent, err := runtime.engine.SendMessage(t.Context(), team.ID, SendMessageRequest{
		Command: runtime.member("lead", team.Revision), MessageID: "durable-message",
		RecipientID: "worker", Body: ai.JSON(`{"text":"persisted"}`),
	})
	require.NoError(t, err)

	reopenedStore, err := NewJSONLStore(dir)
	require.NoError(t, err)
	reopened, err := New(reopenedStore)
	require.NoError(t, err)
	page, err := reopened.Mailbox(t.Context(), team.ID, "worker", MailboxOptions{})
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	assert.Equal(t, MessageID("durable-message"), page.Messages[0].ID)

	acknowledged, err := reopened.AcknowledgeMessages(t.Context(), team.ID, AcknowledgeMessagesRequest{
		Command: CommandMetadata{
			ID: "acknowledge-after-restart", ExpectedRevision: sent.Team.Revision,
			Actor: Actor{Kind: ActorKindMember, ID: "worker"},
		},
		ThroughSequence: page.NextAfter,
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), acknowledged.Members[1].MailboxAcknowledged)

	reopenedAgainStore, err := NewJSONLStore(dir)
	require.NoError(t, err)
	reopenedAgain, err := New(reopenedAgainStore)
	require.NoError(t, err)
	loaded, err := reopenedAgain.Get(t.Context(), team.ID)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), loaded.Members[1].MailboxAcknowledged)
}
