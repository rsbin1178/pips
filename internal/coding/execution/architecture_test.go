package execution_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodingProcessArchitecture(t *testing.T) {
	t.Parallel()

	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)

	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	oldFrontendPath := strings.Join([]string{"internal", "coding", "frontend"}, "/")

	for _, productRoot := range []string{"cmd", "internal"} {
		root := filepath.Join(repositoryRoot, productRoot)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			require.NoError(t, walkErr)

			if entry.IsDir() || filepath.Ext(path) != ".go" {
				return nil
			}

			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			require.NoError(t, err)

			relative, err := filepath.Rel(repositoryRoot, path)
			require.NoError(t, err)

			relative = filepath.ToSlash(relative)

			for _, imported := range file.Imports {
				name, err := strconv.Unquote(imported.Path.Value)
				require.NoError(t, err)

				assert.NotContains(t, name, oldFrontendPath, relative)

				if name == "os/exec" {
					assert.True(
						t,
						strings.HasPrefix(relative, "internal/coding/execution/"),
						"%s imports os/exec outside the execution boundary",
						relative,
					)
				}
			}

			return nil
		})
		require.NoError(t, err)
	}
}
