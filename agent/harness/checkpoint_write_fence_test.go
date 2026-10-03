package harness

import (
	"errors"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The record reaches storage, but its acknowledgement fails (for example fsync
// returns an error). Session cannot know whether the checkpoint is durable.
type checkpointAcknowledgementStore struct {
	Store
	failure   error
	attempts  int
	preflight bool
}

func (s *checkpointAcknowledgementStore) Append(entry Entry) error {
	s.attempts++
	if entry.Kind == KindContextCheckpoint && s.preflight {
		return errors.Join(ErrStoreAppendNotAttempted, s.failure)
	}

	if err := s.Store.Append(entry); err != nil {
		return err
	}

	if entry.Kind == KindContextCheckpoint {
		return s.failure
	}

	return nil
}

func TestContextCheckpointUncertainWriteFencesCleanupAppends(t *testing.T) {
	t.Parallel()

	failure := errors.New("checkpoint sync failed")
	store := &checkpointAcknowledgementStore{Store: NewMemoryStore("uncertain"), failure: failure}
	session, err := NewSession(store)
	require.NoError(t, err)
	leaf := appendCheckpointMessage(t, session, ai.UserText("original task"), nil)
	before := session.Entries()
	_, err = session.AppendContextCheckpoint(leaf, testCheckpoint(), 100)
	require.ErrorIs(t, err, failure)
	assert.Equal(t, before, session.Entries())
	assert.Equal(t, leaf, session.LeafID())

	attempts := store.attempts

	// This is the same append boundary used by parent/child completion journals.
	_, err = session.AppendCustom("completion", ai.JSON(`{"status":"failed"}`))
	require.ErrorIs(t, err, failure)
	_, err = session.AppendMessage(ai.UserText("must not continue"), nil)
	require.ErrorIs(t, err, failure)
	require.ErrorIs(t, session.MoveTo(leaf, ""), failure)
	_, err = session.AppendContextCheckpoint(leaf, testCheckpoint(), 100)
	require.ErrorIs(t, err, failure)
	assert.Equal(t, attempts, store.attempts, "no cleanup, navigation or retry may touch the uncertain store")
	assert.Equal(t, before, session.Entries())

	// A separately reopened, validated transcript may reveal that publication
	// succeeded. No cleanup entry may have branched around that checkpoint.
	reopened, err := NewSession(store.Store)
	require.NoError(t, err)
	visible, err := reopened.Context()
	require.NoError(t, err)
	assert.Equal(t, testCheckpoint().Messages, visible.Messages)
	assert.Len(t, reopened.Entries(), len(before)+1)
}

func TestContextCheckpointStorePreflightDoesNotFenceWrites(t *testing.T) {
	t.Parallel()

	store := &checkpointAcknowledgementStore{
		Store: NewMemoryStore("preflight"), failure: errors.New("size limit"), preflight: true,
	}
	value, err := NewSession(store)
	require.NoError(t, err)
	leaf := appendCheckpointMessage(t, value, ai.UserText("task"), nil)
	_, err = value.AppendContextCheckpoint(leaf, testCheckpoint(), 100)
	require.ErrorIs(t, err, ErrStoreAppendNotAttempted)
	require.Len(t, value.Entries(), 1)
	_, err = value.AppendCustom("completion", ai.JSON(`{"status":"failed"}`))
	require.NoError(t, err, "a proven pre-write rejection does not poison the writer")
	require.Len(t, value.Entries(), 2)
}

func TestContextCheckpointPrevalidationDoesNotFenceWrites(t *testing.T) {
	t.Parallel()

	session, err := NewSession(NewMemoryStore("prevalidation"))
	require.NoError(t, err)
	leaf := appendCheckpointMessage(t, session, ai.UserText("task"), nil)
	_, err = session.AppendContextCheckpoint("stale", testCheckpoint(), 100)
	require.ErrorIs(t, err, ErrStaleContextCheckpoint)
	_, err = session.AppendContextCheckpoint(leaf, testCheckpoint(), -1)
	require.ErrorIs(t, err, ErrInvalidEntry)
	appendCheckpointMessage(t, session, ai.AssistantText("safe to continue"), nil)
}
