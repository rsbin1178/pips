package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoctorCapabilityValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "runtime", value: "bubblewrap", want: "bubblewrap"},
		{name: "version", value: "0.8.0", want: "0.8.0"},
		{name: "empty", want: "unknown"},
		{name: "line injection", value: "bubblewrap\ncredential=secret", want: "unknown"},
		{name: "oversize", value: string(make([]byte, 65)), want: "unknown"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, doctorCapabilityValue(test.value))
		})
	}
}

func TestEnsureDoctorTempRoot(t *testing.T) {
	t.Parallel()

	t.Run("creates owner-only directory", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "nested", "tmp")
		require.NoError(t, ensureDoctorTempRoot(path))
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	})

	t.Run("rejects public directory", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "tmp")
		require.NoError(t, os.Mkdir(path, 0o755))
		require.ErrorContains(t, ensureDoctorTempRoot(path), "owner-only")
	})

	t.Run("rejects symbolic link", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		target := filepath.Join(root, "target")
		require.NoError(t, os.Mkdir(target, 0o700))
		link := filepath.Join(root, "tmp")
		require.NoError(t, os.Symlink(target, link))
		require.ErrorContains(t, ensureDoctorTempRoot(link), "owner-only")
	})
}
