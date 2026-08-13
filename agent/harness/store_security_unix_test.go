//go:build darwin || linux

package harness_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestJSONLReadersRejectSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	store, err := harness.CreateJSONL(target, "target", nil)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	link := filepath.Join(dir, "link.jsonl")
	require.NoError(t, os.Symlink(target, link))

	_, err = harness.OpenJSONL(link)
	require.Error(t, err)
	_, err = harness.ReadJSONLMetadata(link)
	require.Error(t, err)
	_, err = harness.ReadJSONLPrefix(link, harness.JSONLPrefixLimits{})
	require.Error(t, err)

	metas, err := (harness.Repo{Dir: dir}).List()
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, "target", metas[0].ID)
}

func TestJSONLAppendRejectsReplacedPathWithoutModifyingEitherFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	store, err := harness.CreateJSONL(path, "session", nil)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	opened, err := harness.OpenJSONL(path)
	require.NoError(t, err)

	defer opened.Close() //nolint:errcheck // test cleanup

	originalPath := filepath.Join(dir, "original.jsonl")
	require.NoError(t, os.Rename(path, originalPath))

	replacement := []byte("replacement sentinel\n")
	require.NoError(t, os.WriteFile(path, replacement, 0o600))
	original := mustReadFile(t, originalPath)

	sess, err := harness.NewSession(opened)
	require.NoError(t, err)
	_, err = sess.AppendMessage(ai.UserText("must not persist"), nil)
	require.Error(t, err)
	assert.Equal(t, original, mustReadFile(t, originalPath))
	assert.Equal(t, replacement, mustReadFile(t, path))
}

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
