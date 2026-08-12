//go:build darwin || linux

package teamstate_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestStoreRejectsConcurrentProcessWriterLock(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	first := snapshot("team-locked", "s-parent", 1)
	_, err := store.Commit(t.Context(), teamstate.Mutation{
		CommandID: "create", ExpectedRevision: 0, Snapshot: first,
	})
	require.NoError(t, err)

	path := filepath.Join(store.Dir(), "team-locked.jsonl")
	file, err := os.OpenFile(path, os.O_RDWR, 0o600) //nolint:gosec // Test-owned journal.
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	t.Cleanup(func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	})

	secondStore, err := teamstate.New(store.Dir(), teamstate.Limits{})
	require.NoError(t, err)

	second := first
	second.Revision = 2
	second.State = teamstate.StateActive
	second.UpdatedAt = second.UpdatedAt.Add(time.Minute)
	_, err = secondStore.Commit(t.Context(), teamstate.Mutation{
		CommandID: team.CommandID("activate"), ExpectedRevision: 1, Snapshot: second,
	})
	require.ErrorIs(t, err, teamstate.ErrLocked)
}
