package heapprofile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureWritesAnAtomicGzipProfile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "profiles", "heap.pprof")
	require.NoError(t, Capture(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, data)
	assert.True(t, bytes.HasPrefix(data, []byte{0x1f, 0x8b}), "pprof writes a gzip stream")

	// A second capture replaces the first without leaving temporary files.
	require.NoError(t, Capture(path))
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "heap.pprof", entries[0].Name())
}

func TestCaptureWithoutPathIsANoOp(t *testing.T) {
	t.Parallel()

	require.NoError(t, Capture(""))
	require.NoError(t, Capture("   "))
}

func TestStartCapturesOnExitWhenConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heap.pprof")
	t.Setenv(EnvPath, path)

	stop := Start()
	stop()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(data, []byte{0x1f, 0x8b}))
}

func TestStartWithoutEnvironmentIsANoOp(t *testing.T) {
	t.Setenv(EnvPath, "")

	stop := Start()
	stop()
}
