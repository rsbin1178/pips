package cli

import (
	"context"
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

func TestParseScutilHTTPSProxy(t *testing.T) {
	t.Parallel()

	enabled := "<dictionary> {\n  HTTPEnable : 1\n  HTTPPort : 8080\n  HTTPProxy : 10.0.0.2\n" +
		"  HTTPSEnable : 1\n  HTTPSPort : 7890\n  HTTPSProxy : 127.0.0.1\n}\n"
	assert.Equal(t, "127.0.0.1:7890", parseScutilHTTPSProxy(enabled))
	assert.Equal(t, "[::1]:7890", parseScutilHTTPSProxy("HTTPSEnable : 1\nHTTPSPort : 7890\nHTTPSProxy : ::1\n"))
	assert.Empty(t, parseScutilHTTPSProxy("HTTPSEnable : 0\nHTTPSPort : 7890\nHTTPSProxy : 127.0.0.1\n"))
	assert.Empty(t, parseScutilHTTPSProxy("HTTPSEnable : 1\nHTTPSProxy : 127.0.0.1\n"))
	assert.Empty(t, parseScutilHTTPSProxy(""))
}

func TestDoctorProxyStatus(t *testing.T) {
	t.Parallel()

	system := func(context.Context) string { return "127.0.0.1:7890" }
	env := func(values map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			value, ok := values[name]

			return value, ok
		}
	}

	status := doctorProxyStatus(t.Context(), Dependencies{
		LookupEnv:   env(map[string]string{"https_proxy": "http://user:secret@proxy:3128"}),
		SystemProxy: system,
	})
	assert.Equal(t, "network ok proxy=environment variable=https_proxy\n", status)

	status = doctorProxyStatus(t.Context(), Dependencies{
		LookupEnv: env(map[string]string{"HTTPS_PROXY": "", "ALL_PROXY": "http://proxy:3128"}), SystemProxy: system,
	})
	assert.Contains(t, status, "network warning proxy=none system_proxy=127.0.0.1:7890\n")
	assert.Contains(t, status, "export HTTPS_PROXY=http://127.0.0.1:7890")

	status = doctorProxyStatus(t.Context(), Dependencies{
		LookupEnv: env(nil), SystemProxy: func(context.Context) string { return "" },
	})
	assert.Equal(t, "network ok proxy=none\n", status)
}
