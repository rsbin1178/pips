package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// admittedFixture is one read service with a real workspace tree and one
// admitted task log outside it.
type admittedFixture struct {
	service *service
	root    string
	logPath string
}

func newAdmittedFixture(t *testing.T, limits Limits, admittedBytes int64) admittedFixture {
	t.Helper()

	root := t.TempDir()

	opened, err := workspace.Open(root)
	require.NoError(t, err)

	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)

	t.Cleanup(func() { _ = tree.Close() })

	logPath := filepath.Join(t.TempDir(), "terminal", "monitor-1-token.log")
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))

	return admittedFixture{
		service: &service{
			tree:   tree,
			limits: limits,
			taskLog: &TaskLogRead{
				Admit: func(value string) (string, bool) {
					return logPath, value == logPath
				},
				Bytes: admittedBytes,
			},
		},
		root:    root,
		logPath: logPath,
	}
}

func (f admittedFixture) readText(t *testing.T, args readArgs) (string, error) {
	t.Helper()

	parts, err := f.service.read(t.Context(), args)
	if err != nil {
		return "", err
	}

	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			return text.Text, nil
		}
	}

	return "", nil
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestReadToolAdmitsOneTaskLogByExactPath(t *testing.T) {
	t.Parallel()

	fixture := newAdmittedFixture(t, DefaultLimits(), 1<<20)
	writeFile(t, fixture.logPath, "line-1\nline-2\nline-3\n")

	text, err := fixture.readText(t, readArgs{Path: fixture.logPath})
	require.NoError(t, err)

	header, body, err := ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.Contains(t, body, "1: line-1")
	assert.Contains(t, body, "3: line-3")

	// The same line window a workspace read supports.
	offset, limit := 2, 1

	text, err = fixture.readText(t, readArgs{
		Path: fixture.logPath, Offset: &offset, Limit: &limit,
	})
	require.NoError(t, err)

	_, body, err = ParseResult(text)
	require.NoError(t, err)
	assert.Contains(t, body, "2: line-2")
	assert.NotContains(t, body, "1: line-1")
}

// TestReadToolReadsAnAdmittedLogUnderItsOwnBound proves the admitted log is read
// under the registry's output bound, not the workspace file bound: a log at that
// bound still reads while a workspace file of the same size does not.
func TestReadToolReadsAnAdmittedLogUnderItsOwnBound(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.FileBytes = 8

	fixture := newAdmittedFixture(t, limits, 65)
	writeFile(t, fixture.logPath, strings.Repeat("x", 64)+"\n")

	text, err := fixture.readText(t, readArgs{Path: fixture.logPath})
	require.NoError(t, err)

	header, _, err := ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)

	workspacePath := filepath.Join(fixture.root, "big.txt")
	writeFile(t, workspacePath, strings.Repeat("y", 64)+"\n")

	_, err = fixture.readText(t, readArgs{Path: "big.txt"})
	require.Error(t, err)

	header, _, err = ParseResult(err.Error())
	require.NoError(t, err)
	assert.Equal(t, "too_large", header.Code)
}

// TestReadToolRefusesEveryOtherPath keeps the admission narrow: only the exact
// admitted path resolves, and everything else is refused as an
// outside-workspace path.
func TestReadToolRefusesEveryOtherPath(t *testing.T) {
	t.Parallel()

	fixture := newAdmittedFixture(t, DefaultLimits(), 1<<20)
	writeFile(t, fixture.logPath, "line-1\n")

	sibling := filepath.Join(filepath.Dir(fixture.logPath), "monitor-2-token.log")
	writeFile(t, sibling, "other\n")

	otherDirectory := filepath.Join(t.TempDir(), "terminal", "monitor-1-token.log")
	writeFile(t, otherDirectory, "elsewhere\n")

	unknownID := filepath.Join(filepath.Dir(fixture.logPath), "monitor-9-token.log")
	writeFile(t, unknownID, "unknown\n")

	cases := map[string]string{
		"a sibling log":        sibling,
		"another directory":    otherDirectory,
		"a traversed spelling": filepath.Dir(fixture.logPath) + "/../terminal/monitor-1-token.log",
		"an unknown id's log":  unknownID,
		"the log's directory":  filepath.Dir(fixture.logPath),
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := fixture.readText(t, readArgs{Path: value})
			require.Error(t, err)

			header, _, parseErr := ParseResult(err.Error())
			require.NoError(t, parseErr)
			assert.Equal(t, "outside_workspace", header.Code)
		})
	}

	// A workspace file is unaffected by the admission.
	writeFile(t, filepath.Join(fixture.root, "notes.txt"), "workspace line\n")

	text, err := fixture.readText(t, readArgs{Path: "notes.txt"})
	require.NoError(t, err)

	_, body, err := ParseResult(text)
	require.NoError(t, err)
	assert.Contains(t, body, "1: workspace line")
}

// TestReadToolWithoutATaskLogBoundaryRefusesTheLog pins the boundary's absence:
// with no registry configured, the log path is refused like any other
// outside-workspace path.
func TestReadToolWithoutATaskLogBoundaryRefusesTheLog(t *testing.T) {
	t.Parallel()

	fixture := newAdmittedFixture(t, DefaultLimits(), 1<<20)
	writeFile(t, fixture.logPath, "line-1\n")

	fixture.service.taskLog = nil

	_, err := fixture.readText(t, readArgs{Path: fixture.logPath})
	require.Error(t, err)

	header, _, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "outside_workspace", header.Code)
}

// TestReadToolRefusesASymlinkAtTheAdmittedPath pins the workspace read's rule
// onto the admitted log: the path must be the regular file itself, so a link
// placed there cannot make its target readable through the admission.
func TestReadToolRefusesASymlinkAtTheAdmittedPath(t *testing.T) {
	t.Parallel()

	fixture := newAdmittedFixture(t, DefaultLimits(), 1<<20)
	writeFile(t, fixture.logPath, "real-log\n")

	secret := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, secret, "SECRET-CONTENT\n")

	require.NoError(t, os.Remove(fixture.logPath))
	require.NoError(t, os.Symlink(secret, fixture.logPath))

	_, err := fixture.readText(t, readArgs{Path: fixture.logPath})
	require.Error(t, err)

	header, body, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "symlink_not_allowed", header.Code)
	assert.NotContains(t, body, "SECRET-CONTENT")
}

// TestReadToolRefusesABinaryAdmittedLog keeps the binary sample on the admitted
// path too: a log is read exactly as a workspace file is.
func TestReadToolRefusesABinaryAdmittedLog(t *testing.T) {
	t.Parallel()

	fixture := newAdmittedFixture(t, DefaultLimits(), 1<<20)
	writeFile(t, fixture.logPath, "line-1\n\x00binary\n")

	_, err := fixture.readText(t, readArgs{Path: fixture.logPath})
	require.Error(t, err)

	header, _, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "binary_file", header.Code)
}

// TestReadToolRefusesAnAdmittedDirectory pins the regular-file check: an
// admitted value that is not a file is refused before it is read.
func TestReadToolRefusesAnAdmittedDirectory(t *testing.T) {
	t.Parallel()

	fixture := newAdmittedFixture(t, DefaultLimits(), 1<<20)

	directory := filepath.Join(t.TempDir(), "terminal")
	require.NoError(t, os.MkdirAll(directory, 0o700))

	fixture.service.taskLog = &TaskLogRead{
		Admit: func(value string) (string, bool) { return directory, value == directory },
		Bytes: 1 << 20,
	}

	_, err := fixture.readText(t, readArgs{Path: directory})
	require.Error(t, err)

	header, _, parseErr := ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "unsupported_file", header.Code)
}

// TestReadToolAdmitsAWhitespacePaddedExactPath pins the one tolerance the
// admission has: the requested value is trimmed before the exact match, so
// padding around the exact log path resolves to the same file.
func TestReadToolAdmitsAWhitespacePaddedExactPath(t *testing.T) {
	t.Parallel()

	fixture := newAdmittedFixture(t, DefaultLimits(), 1<<20)
	writeFile(t, fixture.logPath, "line-1\n")

	text, err := fixture.readText(t, readArgs{
		Path: "  " + fixture.logPath + "\n",
	})
	require.NoError(t, err)

	_, body, err := ParseResult(text)
	require.NoError(t, err)
	assert.Contains(t, body, "1: line-1")
}

// TestReadToolBoundsAnAdmittedLogsOutput keeps the workspace output bound on the
// admitted path: the same bounded streaming decides what reaches the model.
func TestReadToolBoundsAnAdmittedLogsOutput(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.OutputBytes = 64

	fixture := newAdmittedFixture(t, limits, 1<<20)
	writeFile(t, fixture.logPath, strings.Repeat("x", 40)+"\n"+strings.Repeat("y", 40)+"\n")

	text, err := fixture.readText(t, readArgs{Path: fixture.logPath})
	require.NoError(t, err)

	header, body, err := ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Equal(t, "bytes", header.Reason)
	require.NotNil(t, header.Next.Offset)
	assert.Equal(t, 2, *header.Next.Offset)
	assert.LessOrEqual(t, len(body), limits.OutputBytes)
}
