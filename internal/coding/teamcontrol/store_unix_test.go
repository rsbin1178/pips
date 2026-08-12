//go:build darwin || linux

package teamcontrol_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/rsbin1178/pips/internal/coding/teamcontrol"
)

func TestStoreRejectsConcurrentProcessWriter(t *testing.T) {
	t.Parallel()

	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
	require.NoError(t, err)

	first := newCommand("operator-lock-1", "team-lock", "worker-1")
	_, err = store.Submit(t.Context(), first, 0)
	require.NoError(t, err)

	file, err := os.OpenFile(filepath.Join(store.Dir(), "team-lock.jsonl"), os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB))
	t.Cleanup(func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	})

	_, err = store.Submit(t.Context(), newCommand("operator-lock-2", "team-lock", "worker-1"), 1)
	assert.ErrorIs(t, err, teamcontrol.ErrLocked)
}
