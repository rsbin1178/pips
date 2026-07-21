//nolint:wsl_v5 // Acquisition-order tests keep setup adjacent to assertions.
package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInteractiveTerminalFailurePrecedesWorkspaceAcquisition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		terminal TerminalDetector
		lookup   func(string) (string, bool)
	}{
		{
			name: "non tty",
			terminal: func(_ io.Reader, _ io.Writer) (bool, bool) {
				return false, true
			},
			lookup: os.LookupEnv,
		},
		{
			name: "dumb terminal",
			terminal: func(_ io.Reader, _ io.Writer) (bool, bool) {
				return true, true
			},
			lookup: func(name string) (string, bool) {
				if name == "TERM" {
					return "dumb", true
				}

				return "", false
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			layout, err := paths.New(t.TempDir())
			require.NoError(t, err)
			workspaceAcquired := false
			tuiStarted := false
			command, err := New(Dependencies{
				Paths:     layout,
				LookupEnv: test.lookup,
				WorkingDir: func() (string, error) {
					workspaceAcquired = true

					return "", errors.New("must not be called")
				},
				Terminal: test.terminal,
				RunTUI: func(context.Context, tui.Options) error {
					tuiStarted = true

					return nil
				},
			})
			require.NoError(t, err)
			command.SetIn(bytes.NewBuffer(nil))
			command.SetOut(new(bytes.Buffer))

			err = command.ExecuteContext(t.Context())
			require.ErrorIs(t, err, ErrUsage)
			assert.Contains(t, err.Error(), "pips exec")
			assert.False(t, workspaceAcquired)
			assert.False(t, tuiStarted)
		})
	}
}

func TestInteractiveTrustDecisionControlsProjectConfigLoading(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	userRoot := t.TempDir()
	layout, err := paths.New(userRoot)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(layout.ConfigFile(), []byte(`
[model]
provider = "openai"
id = "user-model"
api = "responses"
`), 0o600))

	projectRoot := filepath.Join(workspaceRoot, paths.ProjectRoot())
	require.NoError(t, os.MkdirAll(projectRoot, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceRoot, filepath.FromSlash(paths.ProjectConfigFile())),
		[]byte("this is not toml ="),
		0o600,
	))

	tests := []struct {
		name         string
		trustProject bool
		wantErr      bool
		wantOpened   bool
	}{
		{name: "deny ignores project", trustProject: false, wantOpened: true},
		{name: "allow loads project", trustProject: true, wantErr: true},
	}

	for _, test := range tests { //nolint:paralleltest // The deny/allow sequence shares one trust store.
		t.Run(test.name, func(t *testing.T) {
			opened := false
			command, err := New(Dependencies{
				Paths:      layout,
				WorkingDir: func() (string, error) { return workspaceRoot, nil },
				LookupEnv: func(name string) (string, bool) {
					if name == "API_KEY" {
						return "test-key", true
					}

					return "", false
				},
				Terminal: func(_ io.Reader, _ io.Writer) (bool, bool) {
					return true, true
				},
				OpenControl: func(
					context.Context,
					coding.OpenOptions,
				) (tui.Controller, error) {
					opened = true

					return nil, nil
				},
				RunTUI: func(ctx context.Context, options tui.Options) error {
					assert.False(t, options.Trusted)
					_, bootstrapErr := options.Bootstrap(ctx, test.trustProject)

					return bootstrapErr
				},
			})
			require.NoError(t, err)
			command.SetIn(bytes.NewBuffer(nil))
			command.SetOut(new(bytes.Buffer))

			err = command.ExecuteContext(t.Context())
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, test.wantOpened, opened)
		})
	}
}
