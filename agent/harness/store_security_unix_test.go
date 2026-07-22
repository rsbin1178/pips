//go:build darwin || linux

package harness_test

import (
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/agent/harness"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestJSONLReadersRejectFIFO(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, unix.Mkfifo(path, 0o600))

	_, err := harness.OpenJSONL(path)
	require.Error(t, err)
	_, err = harness.ReadJSONLMetadata(path)
	require.Error(t, err)
	_, err = harness.ReadJSONLPrefix(path, harness.JSONLPrefixLimits{})
	require.Error(t, err)

	metas, err := (harness.Repo{Dir: filepath.Dir(path)}).List()
	require.NoError(t, err)
	require.Empty(t, metas)
}
