//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package pluginstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rsbin/pips/internal/coding/pluginstore"
	"github.com/stretchr/testify/require"
)

func TestStoreRejectsNonExecutableSourceArtifact(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o700))

	artifact := []byte("plugin")
	artifactPath := filepath.Join(packageRoot, "plugin.bin")
	require.NoError(t, os.WriteFile(artifactPath, artifact, 0o600))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))

	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	_, err = store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{})
	require.ErrorIs(t, err, pluginstore.ErrUnsafeArtifact)
	require.Contains(t, err.Error(), "execute bit")
}
