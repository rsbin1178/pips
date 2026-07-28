package gitcontrol

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunnerBuildsFixedWorktreeArgv(t *testing.T) {
	t.Parallel()

	runner := testRunner(t)

	var calls [][]string

	runner.process = func(
		_ context.Context,
		_ string,
		arguments, _ []string,
		_ []byte,
		_ int64,
		_ time.Duration,
	) (commandResult, error) {
		calls = append(calls, append([]string(nil), arguments...))

		return commandResult{}, nil
	}

	repository := filepath.Clean(t.TempDir())
	worktree := filepath.Join(repository, "attempt")
	require.NoError(t, runner.AddWorktree(
		t.Context(), repository, worktree,
		"refs/heads/pips/team/a/b/c", "pips/team-worktree/v1 team=a attempt=c",
	))
	require.NoError(t, runner.RemoveWorktree(t.Context(), repository, worktree))

	prefix := append([]string{"-C", repository}, fixedConfig...)
	assert.Equal(t, append(append([]string(nil), prefix...),
		"worktree", "add", "--no-checkout", "--lock", "--reason",
		"pips/team-worktree/v1 team=a attempt=c", "--", worktree,
		"pips/team/a/b/c",
	), calls[0])
	assert.Equal(t, append(append([]string(nil), prefix...),
		"worktree", "remove", "--", worktree,
	), calls[1])
}

func TestRunnerBuildsReadOnlyStatusArgvAndDigest(t *testing.T) {
	t.Parallel()

	runner := testRunner(t)

	var call []string

	runner.process = func(
		_ context.Context,
		_ string,
		arguments, _ []string,
		_ []byte,
		_ int64,
		_ time.Duration,
	) (commandResult, error) {
		call = append([]string(nil), arguments...)

		return commandResult{stdout: []byte("?? untracked\x00")}, nil
	}

	repository := filepath.Clean(t.TempDir())
	status, err := runner.SnapshotStatus(t.Context(), repository)
	require.NoError(t, err)
	assert.False(t, status.Clean)
	assert.Len(t, status.Digest, 64)
	assert.Equal(t, []string{"untracked"}, status.Paths)

	prefix := append([]string{"-C", repository}, fixedConfig...)
	assert.Equal(t, append(append([]string(nil), prefix...),
		"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none",
	), call)
}

func TestParseStatusPathsIncludesBothRenameSides(t *testing.T) {
	t.Parallel()

	paths, err := parseStatusPaths(
		[]byte(" M changed.txt\x00R  destination.txt\x00source.txt\x00?? untracked.txt\x00"),
		10,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"changed.txt", "destination.txt", "source.txt", "untracked.txt",
	}, paths)
}

func TestRunnerBuildsAtomicRefTransaction(t *testing.T) {
	t.Parallel()

	runner := testRunner(t)

	var input []byte

	runner.process = func(
		_ context.Context,
		_ string,
		_ []string,
		_ []string,
		value []byte,
		_ int64,
		_ time.Duration,
	) (commandResult, error) {
		input = append([]byte(nil), value...)

		return commandResult{}, nil
	}

	oldOID := "1111111111111111111111111111111111111111"
	newOID := "2222222222222222222222222222222222222222"
	require.NoError(t, runner.UpdateRefs(
		t.Context(), filepath.Clean(t.TempDir()), "sha1", "pips result",
		[]RefUpdate{
			{Ref: "refs/pips/team/a/results/b", NewOID: newOID, Create: true},
			{Ref: "refs/heads/pips/team/a/b/c", NewOID: newOID, OldOID: oldOID},
		},
	))
	assert.Equal(t,
		"update refs/pips/team/a/results/b "+newOID+" 0000000000000000000000000000000000000000\n"+
			"update refs/heads/pips/team/a/b/c "+newOID+" "+oldOID+"\n",
		string(input),
	)
}

func TestRunnerSafeConfigScanIncludesLocalIncludes(t *testing.T) {
	t.Parallel()

	runner := testRunner(t)

	var calls [][]string

	runner.process = func(
		_ context.Context,
		_ string,
		arguments, _ []string,
		_ []byte,
		_ int64,
		_ time.Duration,
	) (commandResult, error) {
		calls = append(calls, append([]string(nil), arguments...))
		if len(calls) == 1 {
			return commandResult{}, &processExitError{Code: 1}
		}

		return commandResult{stdout: []byte("filter.evil.clean\x00/bin/evil\x00")}, nil
	}

	repository := filepath.Clean(t.TempDir())
	err := runner.ValidateSafeConfig(t.Context(), repository)
	require.ErrorIs(t, err, ErrUnsafeConfig)
	require.Len(t, calls, 2)

	prefix := append([]string{"-C", repository}, fixedConfig...)
	assert.Equal(t, append(append([]string(nil), prefix...),
		"config", "--local", "--bool", "--get", "extensions.worktreeConfig",
	), calls[0])
	assert.Equal(t, append(append([]string(nil), prefix...),
		"config", "--includes", "--local", "--null", "--get-regexp",
		`^(filter\..*\.(clean|smudge|process)|diff\..*\.(command|textconv)|core\.fsmonitor|core\.hooksPath|gpg\..*\.program|user\.signingKey)$`,
	), calls[1])
}

func TestParseWorktreesPorcelainZ(t *testing.T) {
	t.Parallel()

	value := []byte(
		"worktree /repo\x00HEAD 1111111111111111111111111111111111111111\x00" +
			"branch refs/heads/main\x00\x00" +
			"worktree /repo/wt\x00HEAD 2222222222222222222222222222222222222222\x00" +
			"branch refs/heads/topic\x00locked pips owner\x00\x00",
	)
	worktrees, err := parseWorktrees(value, 10)
	require.NoError(t, err)
	require.Len(t, worktrees, 2)
	assert.Equal(t, "/repo/wt", worktrees[1].Path)
	assert.True(t, worktrees[1].Locked)
	assert.Equal(t, "pips owner", worktrees[1].LockReason)
}

func TestGitControlSourceContainsNoDestructiveFallbackLiteral(t *testing.T) {
	t.Parallel()

	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)

	directory := filepath.Dir(source)
	forbidden := map[string]struct{}{
		"--force": {}, "-f": {}, "-B": {}, "-D": {}, "prune": {},
		"merge": {}, "cherry-pick": {}, "reset": {}, "clean": {}, "stash": {}, "apply": {},
		"sh": {}, "bash": {}, "zsh": {},
	}

	entries, err := os.ReadDir(directory)
	require.NoError(t, err)

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		file, err := parser.ParseFile(
			token.NewFileSet(), filepath.Join(directory, entry.Name()), nil, 0,
		)
		require.NoError(t, err)
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok {
				assert.NotEqual(t, "RemoveAll", selector.Sel.Name,
					"%s contains broad recursive cleanup", entry.Name())
			}

			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}

			value, err := strconv.Unquote(literal.Value)
			require.NoError(t, err)

			_, found := forbidden[value]
			assert.False(t, found, "%s contains forbidden command literal %q", entry.Name(), value)

			return true
		})
	}
}

func TestGitControlExportsNoFreeArgvOperation(t *testing.T) {
	t.Parallel()

	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)

	directory := filepath.Dir(source)
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		file, err := parser.ParseFile(
			token.NewFileSet(), filepath.Join(directory, entry.Name()), nil, 0,
		)
		require.NoError(t, err)

		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() || function.Type.Params == nil {
				continue
			}

			for _, field := range function.Type.Params.List {
				ellipsis, variadic := field.Type.(*ast.Ellipsis)
				if !variadic {
					continue
				}

				identifier, stringsArgv := ellipsis.Elt.(*ast.Ident)
				assert.False(t, stringsArgv && identifier.Name == "string",
					"%s exports a free argv variadic", function.Name.Name)
			}
		}
	}
}

func testRunner(t *testing.T) *Runner {
	t.Helper()

	gitPath := "/usr/bin/git"
	if _, err := os.Stat(gitPath); err != nil {
		t.Skip("system Git is unavailable")
	}

	runner, err := New(gitPath, DefaultLimits())
	require.NoError(t, err)

	return runner
}
