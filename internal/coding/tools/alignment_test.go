//nolint:wsl_v5 // Fixture setup stays beside its assertions.
package tools_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLSRecursiveTreeAndSummaries(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("README.md", "readme")
	fixture.write("docs/a.md", "a")
	fixture.write("docs/b.txt", "b")
	fixture.write("docs/c.txt", "c")
	fixture.write("src/main.go", "package main")
	fixture.write("src/lib.go", "package main")
	fixture.write("src/util/helpers.go", "package util")

	text, err := fixture.exec(t.Context(), "ls", `{}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.OK)
	assert.False(t, header.Truncated)
	assert.Equal(t, "- ./\n"+
		"  - docs/\n"+
		"    - a.md\n"+
		"    - b.txt\n"+
		"    - c.txt\n"+
		"  - README.md\n"+
		"  - src/\n"+
		"    - lib.go\n"+
		"    - main.go\n"+
		"    - util/\n"+
		"      - helpers.go\n", body)
}

func TestLSBFSCollapsesOversizedSiblingAndExpandsLater(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.ListOutputChars = 200
	fixture := newToolFixture(t, limits)
	for index := range 20 {
		fixture.write(filepath.Join("aaa", "file"+strconv.Itoa(index)+".txt"), "x")
	}
	fixture.write("zzz/only.txt", "x")

	text, err := fixture.exec(t.Context(), "ls", `{}`)
	require.NoError(t, err)
	_, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Contains(t, body, "[20 files in subtree: 20 *.txt]")
	assert.Contains(t, body, "  - zzz/\n    - only.txt\n")
}

func TestLSHidesDotGitAndIgnoredFiles(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	require.NoError(t, os.Mkdir(filepath.Join(fixture.root, ".git"), 0o700))
	fixture.write(".gitignore", "*.log\n!keep.log\n/rootonly.txt\nbuild/\nlogname\nsub/*.tmp\n")
	fixture.write(".ignore", "ignored.txt\nkeep.log\n")
	fixture.write("sub/.gitignore", "!keep.tmp\n")
	fixture.write("sub/drop.tmp", "drop")
	fixture.write("sub/keep.tmp", "keep")
	fixture.write("drop.log", "drop")
	fixture.write("keep.log", "keep")
	fixture.write("rootonly.txt", "root")
	fixture.write("sub/rootonly.txt", "nested")
	fixture.write("ignored.txt", "ignored")
	fixture.write("visible.txt", "visible")
	fixture.write(".secret", "hidden")
	fixture.write("build/out.txt", "build")
	fixture.write("nested/logname", "log")

	text, err := fixture.exec(t.Context(), "ls", `{}`)
	require.NoError(t, err)
	_, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Contains(t, body, "visible.txt")
	assert.Contains(t, body, "sub/")
	assert.Contains(t, body, "rootonly.txt")
	assert.Contains(t, body, "keep.tmp")
	assert.NotContains(t, body, "drop.tmp")
	assert.NotContains(t, body, "drop.log")
	assert.NotContains(t, body, "keep.log", ".ignore overrides the .gitignore negation")
	assert.NotContains(t, body, "ignored.txt")
	assert.NotContains(t, body, "build/")
	assert.NotContains(t, body, "logname")
	assert.NotContains(t, body, ".secret")
	assert.NotContains(t, body, ".git/")
}

func TestLSBudgetRootTruncationAndItemCap(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.ListOutputChars = 60
	fixture := newToolFixture(t, limits)
	for index := range 12 {
		fixture.write(filepath.Join("directory_"+strconv.Itoa(index), "file.txt"), "x")
	}

	text, err := fixture.exec(t.Context(), "ls", `{}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Equal(t, "bytes", header.Reason)
	assert.Contains(t, body, "this directory is too large to list fully")

	limits = tools.DefaultLimits()
	limits.ListTreeItems = 3
	fixture = newToolFixture(t, limits)
	for index := range 8 {
		fixture.write("file-"+strconv.Itoa(index)+".txt", "x")
	}

	text, err = fixture.exec(t.Context(), "ls", `{}`)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Contains(t, body, "more than 3 items")
}

func TestLSTypedErrors(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("file.txt", "x")
	require.NoError(t, os.Symlink("file.txt", filepath.Join(fixture.root, "link")))

	cases := []struct {
		name string
		args string
		code string
	}{
		{"missing", `{"path":"missing"}`, "not_found"},
		{"file", `{"path":"file.txt"}`, "is_a_file"},
		{"intermediate file", `{"path":"file.txt/child"}`, "not_a_directory"},
		{"symlink", `{"path":"link"}`, "symlink_not_allowed"},
		{"outside", `{"path":` + strconv.Quote(filepath.Join(t.TempDir(), "outside")) + `}`, "outside_workspace"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := fixture.exec(t.Context(), "ls", test.args)
			require.Error(t, err)
			header, _, parseErr := tools.ParseResult(err.Error())
			require.NoError(t, parseErr)
			assert.Equal(t, test.code, header.Code)
		})
	}
}

func TestIgnoreSourcesCoverGlobalAndGitExclude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	config := filepath.Join(home, ".gitconfig")
	global := filepath.Join(home, "global-ignore")
	require.NoError(t, os.WriteFile(global, []byte("*.global\n"), 0o600))
	require.NoError(t, os.WriteFile(config, []byte("[core]\n\texcludesFile = "+global+"\n"), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", config)

	fixture := newToolFixture(t, tools.DefaultLimits())
	require.NoError(t, os.MkdirAll(filepath.Join(fixture.root, ".git", "info"), 0o700))
	fixture.write(".git/info/exclude", "*.excluded\n")
	fixture.write("hit.global", "needle\n")
	fixture.write("hit.excluded", "needle\n")
	fixture.write("visible.txt", "needle\n")

	text, err := fixture.exec(t.Context(), "ls", `{}`)
	require.NoError(t, err)
	_, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Contains(t, body, "visible.txt")
	assert.NotContains(t, body, "hit.global")
	assert.NotContains(t, body, "hit.excluded")

	text, err = fixture.exec(t.Context(), "grep", `{"pattern":"needle"}`)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 matching lines\nvisible.txt\n1:needle\n", body)
}

func TestLSPermissionDenied(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root can read a 000 directory")
	}

	fixture := newToolFixture(t, tools.DefaultLimits())
	require.NoError(t, os.Mkdir(filepath.Join(fixture.root, "blocked"), 0o700))
	require.NoError(t, os.Chmod(filepath.Join(fixture.root, "blocked"), 0))
	t.Cleanup(func() {
		//nolint:gosec // Restore the fixture directory before cleanup.
		_ = os.Chmod(filepath.Join(fixture.root, "blocked"), 0o700)
	})

	_, err := fixture.exec(t.Context(), "ls", `{"path":"blocked"}`)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "permission_denied", header.Code)
}

func TestGrepContextGroupingAndOutputModes(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("a.txt", "one\ntwo\nmatch three\nfour\nfive\nsix\nmatch seven\neight\n")

	text, err := fixture.exec(
		t.Context(), "grep",
		`{"pattern":"match","before_context":1,"after_context":1}`,
	)
	require.NoError(t, err)
	_, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "Found 2 matching lines\n"+
		"a.txt\n"+
		"2-two\n"+
		"3:match three\n"+
		"4-four\n"+
		"6-six\n"+
		"7:match seven\n"+
		"8-eight\n", body)

	text, err = fixture.exec(
		t.Context(), "grep", `{"pattern":"match","output_mode":"files_with_matches"}`,
	)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 files\na.txt\n", body)

	text, err = fixture.exec(
		t.Context(), "grep", `{"pattern":"match","output_mode":"count"}`,
	)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "Found 2 across 1 files\na.txt:2\n", body)

	text, err = fixture.exec(t.Context(), "grep", `{"pattern":"match","limit":1}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Equal(t, "matches", header.Reason)
	require.NotNil(t, header.Next.Offset)
	assert.Equal(t, 1, *header.Next.Offset)
	assert.Contains(t, body, "Found at least 1 matching lines")
}

func TestGrepFilesAndCountPagination(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("a.txt", "needle\n")
	fixture.write("b.txt", "needle\nneedle-two\n")

	text, err := fixture.exec(
		t.Context(), "grep",
		`{"pattern":"needle","output_mode":"files_with_matches","limit":1}`,
	)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	require.NotNil(t, header.Next.Offset)
	assert.Equal(t, 1, *header.Next.Offset)
	assert.Contains(t, body, "a.txt")

	text, err = fixture.exec(
		t.Context(), "grep",
		`{"pattern":"needle","output_mode":"files_with_matches","offset":1,"limit":1}`,
	)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated, "the scan stops at the limit without probing the rest of the tree")
	assert.Contains(t, body, "b.txt")

	text, err = fixture.exec(
		t.Context(), "grep",
		`{"pattern":"needle","output_mode":"count","limit":1}`,
	)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Contains(t, body, "a.txt:1")

	text, err = fixture.exec(
		t.Context(), "grep",
		`{"pattern":"needle","output_mode":"count","offset":1,"limit":1}`,
	)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Contains(t, body, "Found 2 across at least 1 files")
	assert.Contains(t, body, "b.txt:2")
}

func TestGrepNoMatches(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("a.txt", "alpha\n")

	text, err := fixture.exec(t.Context(), "grep", `{"pattern":"absent"}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.False(t, header.Truncated)
	assert.Equal(t, "No matches found\n", body)
}

func TestGrepTypeMultilineGlobAndFileRoot(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("a.go", "package a\nfunc foo() {\n}\n")
	fixture.write("b.txt", "foo\n")

	text, err := fixture.exec(t.Context(), "grep", `{"pattern":"foo","type":"go"}`)
	require.NoError(t, err)
	_, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 matching lines\na.go\n2:func foo() {\n", body)

	text, err = fixture.exec(
		t.Context(), "grep", `{"pattern":"foo\\(\\) \\{\\n\\}","multiline":true}`,
	)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Contains(t, body, "a.go\n2:func foo() {\n3:}\n")

	text, err = fixture.exec(t.Context(), "grep", `{"pattern":"foo","glob":"!*.txt"}`)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 matching lines\na.go\n2:func foo() {\n", body)

	text, err = fixture.exec(t.Context(), "grep", `{"pattern":"foo","path":"b.txt"}`)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 matching lines\nb.txt\n1:foo\n", body)
}

func TestGrepSkipsHiddenGitAndIgnoredFiles(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	require.NoError(t, os.Mkdir(filepath.Join(fixture.root, ".git"), 0o700))
	fixture.write(".gitignore", "*.skip\n")
	fixture.write("visible.txt", "needle\n")
	fixture.write("hidden.txt.skip", "needle\n")
	fixture.write(".hidden.txt", "needle\n")
	fixture.write(".git/config", "needle\n")

	text, err := fixture.exec(t.Context(), "grep", `{"pattern":"needle"}`)
	require.NoError(t, err)
	_, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 matching lines\nvisible.txt\n1:needle\n", body)

	text, err = fixture.exec(t.Context(), "grep", `{"pattern":"needle","path":"hidden.txt.skip"}`)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "Found 1 matching lines\nhidden.txt.skip\n1:needle\n", body,
		"an explicit file path bypasses traversal ignore rules")
}

func TestGrepStopsAtByteBudget(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.GrepOutputBytes = 80
	fixture := newToolFixture(t, limits)
	var content strings.Builder
	for index := range 20 {
		content.WriteString("match line " + strconv.Itoa(index) + "\n")
	}
	fixture.write("a.txt", content.String())

	text, err := fixture.exec(t.Context(), "grep", `{"pattern":"match"}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Equal(t, "bytes", header.Reason)
	require.NotNil(t, header.Next.Offset)
	assert.Positive(t, *header.Next.Offset)
	assert.Contains(t, body, "Found at least")
	assert.Contains(t, body, "a.txt")
}

func TestGrepRejectsUnsafeAndTimesOut(t *testing.T) {
	t.Parallel()

	limits := tools.DefaultLimits()
	limits.GrepFileBytes = 16
	fixture := newToolFixture(t, limits)
	fixture.write("binary.txt", "needle\x00binary")
	fixture.write("large.txt", strings.Repeat("needle ", 8))
	fixture.write("visible.txt", "needle\n")
	require.NoError(t, os.Symlink("visible.txt", filepath.Join(fixture.root, "link")))

	text, err := fixture.exec(t.Context(), "grep", `{"pattern":"needle"}`)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, 2, header.Counts.Skipped)
	assert.Equal(t, "Found 1 matching lines\nvisible.txt\n1:needle\n", body)

	for _, test := range []struct {
		name string
		args string
		code string
	}{
		{"symlink", `{"pattern":"needle","path":"link"}`, "symlink_not_allowed"},
		{"outside", `{"pattern":"needle","path":` + strconv.Quote(filepath.Join(t.TempDir(), "outside")) + `}`, "outside_workspace"},
		{"regex", `{"pattern":"["}`, "invalid_argument"},
		{"unknown type", `{"pattern":"needle","type":"not-a-real-type"}`, "invalid_argument"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, execErr := fixture.exec(t.Context(), "grep", test.args)
			require.Error(t, execErr)
			resultHeader, _, parseErr := tools.ParseResult(execErr.Error())
			require.NoError(t, parseErr)
			assert.Equal(t, test.code, resultHeader.Code)
		})
	}

	timeoutLimits := tools.DefaultLimits()
	timeoutLimits.GrepTimeout = 1
	timeoutFixture := newToolFixture(t, timeoutLimits)
	timeoutFixture.write("many.txt", strings.Repeat("needle\n", 1_000))

	text, err = timeoutFixture.exec(t.Context(), "grep", `{"pattern":"needle"}`)
	require.NoError(t, err)
	header, _, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.True(t, header.Truncated)
	assert.Equal(t, "timeout", header.Reason)
}

func TestGrepPermissionDenied(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root can read a 000 file")
	}

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("locked.txt", "needle\n")
	require.NoError(t, os.Chmod(filepath.Join(fixture.root, "locked.txt"), 0))
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(fixture.root, "locked.txt"), 0o600)
	})

	_, err := fixture.exec(t.Context(), "grep", `{"pattern":"needle","path":"locked.txt"}`)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "permission_denied", header.Code)
}

func TestWriteCreatesOverwritesAndPreservesMode(t *testing.T) {
	t.Parallel()

	fixture := newToolFixtureWithStrategy(
		t, tools.DefaultLimits(), config.EditStrategyWriteAndReplace,
	)

	text, err := fixture.exec(
		t.Context(), "Write", `{"file_path":"nested/new.txt","content":"hello\nworld\n"}`,
	)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "A nested/new.txt\n", body)
	require.NotNil(t, header.Edit)
	assert.Equal(t, "create", header.Edit.Kind)
	assert.Equal(t, 2, header.Edit.NewLines)
	assert.Equal(t, "hello\nworld\n", string(mustRead(t, filepath.Join(fixture.root, "nested/new.txt"))))

	fixture.write("existing.txt", "old\n")
	//nolint:gosec // Executable permissions are intentional input to the mode-preservation test.
	require.NoError(t, os.Chmod(filepath.Join(fixture.root, "existing.txt"), 0o751))

	text, err = fixture.exec(
		t.Context(), "Write", `{"file_path":"existing.txt","content":"new\n"}`,
	)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "M existing.txt\n", body)
	require.NotNil(t, header.Edit)
	assert.Equal(t, "overwrite", header.Edit.Kind)
	assert.Equal(t, 1, header.Edit.OldLines)
	info, statErr := os.Stat(filepath.Join(fixture.root, "existing.txt"))
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o751), info.Mode().Perm())

	text, err = fixture.exec(t.Context(), "Write", `{"file_path":"empty.txt","content":""}`)
	require.NoError(t, err)
	_, _, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Empty(t, mustRead(t, filepath.Join(fixture.root, "empty.txt")))
}

func TestWriteRejectsUnsafeContentAndPaths(t *testing.T) {
	t.Parallel()

	fixture := newToolFixtureWithStrategy(
		t, tools.DefaultLimits(), config.EditStrategyWriteAndReplace,
	)
	fixture.write("binary.bin", "old\x00binary")
	require.NoError(t, os.Symlink("binary.bin", filepath.Join(fixture.root, "link")))

	for _, test := range []struct {
		name string
		args string
		code string
	}{
		{"nul content", `{"file_path":"a.txt","content":"a\u0000b"}`, "binary_file"},
		{"binary overwrite", `{"file_path":"binary.bin","content":"new"}`, "binary_file"},
		{"symlink", `{"file_path":"link","content":"new"}`, "symlink_not_allowed"},
		{"long name", `{"file_path":` + strconv.Quote(strings.Repeat("a", 256)) + `,"content":"x"}`, "filename_too_long"},
		{"outside", `{"file_path":` + strconv.Quote(filepath.Join(t.TempDir(), "outside.txt")) + `,"content":"x"}`, "outside_workspace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := fixture.exec(t.Context(), "Write", test.args)
			require.Error(t, err)
			header, _, parseErr := tools.ParseResult(err.Error())
			require.NoError(t, parseErr)
			assert.Equal(t, test.code, header.Code)
		})
	}
}

func TestEditUniqueAllEmptyAndCRLF(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("a.txt", "alpha beta alpha\n")

	_, err := fixture.exec(
		t.Context(), "Edit",
		`{"file_path":"a.txt","old_string":"alpha","new_string":"gamma"}`,
	)
	require.Error(t, err)
	header, _, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "multiple_matches", header.Code)

	text, err := fixture.exec(
		t.Context(), "Edit",
		`{"file_path":"a.txt","old_string":"alpha","new_string":"gamma","replace_all":true}`,
	)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "M a.txt\n", body[:len("M a.txt\n")])
	require.NotNil(t, header.Edit)
	assert.Equal(t, 2, header.Edit.Replacements)
	assert.Equal(t, "gamma beta gamma\n", string(mustRead(t, filepath.Join(fixture.root, "a.txt"))))

	text, err = fixture.exec(
		t.Context(), "Edit",
		`{"file_path":"created.txt","old_string":"","new_string":"created\n"}`,
	)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "A created.txt\n", body)

	text, err = fixture.exec(
		t.Context(), "Edit",
		`{"file_path":"created.txt","old_string":"","new_string":"replaced\n"}`,
	)
	require.NoError(t, err)
	_, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "M created.txt\n", body)
	assert.Equal(t, "replaced\n", string(mustRead(t, filepath.Join(fixture.root, "created.txt"))))

	fixture.write("empty.txt", "")
	text, err = fixture.exec(
		t.Context(), "Edit",
		`{"file_path":"empty.txt","old_string":"","new_string":"filled\n"}`,
	)
	require.NoError(t, err)
	header, body, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "A empty.txt\n", body)
	require.NotNil(t, header.Edit)
	assert.Equal(t, "create", header.Edit.Kind)
	assert.Equal(t, "filled\n", string(mustRead(t, filepath.Join(fixture.root, "empty.txt"))))

	fixture.write("crlf.txt", "hello\r\nworld\r\n")
	text, err = fixture.exec(
		t.Context(), "Edit",
		`{"file_path":"crlf.txt","old_string":"hello\nworld\n","new_string":"goodbye\nearth\n"}`,
	)
	require.NoError(t, err)
	_, _, err = tools.ParseResult(text)
	require.NoError(t, err)
	assert.Equal(t, "goodbye\r\nearth\r\n", string(mustRead(t, filepath.Join(fixture.root, "crlf.txt"))))
}

func TestEditRecoverableErrors(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("a.txt", "one\ntwo\nthree\n")
	require.NoError(t, os.Mkdir(filepath.Join(fixture.root, "dir"), 0o700))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(fixture.root, "link")))

	cases := []struct {
		name string
		args string
		code string
	}{
		{
			"no match",
			`{"file_path":"a.txt","old_string":"two\nmissing","new_string":"x"}`,
			"no_match",
		},
		{
			"directory",
			`{"file_path":"dir","old_string":"a","new_string":"b"}`,
			"is_a_directory",
		},
		{
			"long name",
			`{"file_path":` + strconv.Quote(strings.Repeat("a", 256)) + `,"old_string":"a","new_string":"b"}`,
			"filename_too_long",
		},
		{
			"symlink",
			`{"file_path":"link","old_string":"one","new_string":"x"}`,
			"symlink_not_allowed",
		},
		{
			"outside",
			`{"file_path":` + strconv.Quote(filepath.Join(t.TempDir(), "outside.txt")) + `,"old_string":"a","new_string":"b"}`,
			"outside_workspace",
		},
		{
			"same strings",
			`{"file_path":"a.txt","old_string":"one","new_string":"one"}`,
			"invalid_argument",
		},
		{
			"missing file",
			`{"file_path":"missing.txt","old_string":"one","new_string":"two"}`,
			"not_found",
		},
		{
			"missing parent",
			`{"file_path":"missing/child.txt","old_string":"","new_string":"x"}`,
			"not_found",
		},
		{
			"binary replacement",
			`{"file_path":"a.txt","old_string":"one","new_string":"a\u0000b"}`,
			"binary_file",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := fixture.exec(t.Context(), "Edit", test.args)
			require.Error(t, err)
			header, body, parseErr := tools.ParseResult(err.Error())
			require.NoError(t, parseErr)
			assert.Equal(t, test.code, header.Code)
			if test.code == "no_match" {
				assert.Contains(t, body, "Nearest match: line 2: two")
			}
		})
	}
}

func TestEditPermissionDenied(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root can read a 000 file")
	}

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("locked.txt", "old\n")
	require.NoError(t, os.Chmod(filepath.Join(fixture.root, "locked.txt"), 0))
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(fixture.root, "locked.txt"), 0o600)
	})

	_, err := fixture.exec(
		t.Context(), "Edit",
		`{"file_path":"locked.txt","old_string":"old","new_string":"new"}`,
	)
	require.Error(t, err)
	header, body, parseErr := tools.ParseResult(err.Error())
	require.NoError(t, parseErr)
	assert.Equal(t, "permission_denied", header.Code)
	assert.Contains(t, body, "permission denied reading")
}

func TestEditRoundTripBodyHasContext(t *testing.T) {
	t.Parallel()

	fixture := newToolFixture(t, tools.DefaultLimits())
	fixture.write("a.txt", "one\ntwo\nthree\nfour\nfive\n")

	text, err := fixture.exec(
		t.Context(), "Edit",
		`{"file_path":"a.txt","old_string":"three","new_string":"THREE"}`,
	)
	require.NoError(t, err)
	header, body, err := tools.ParseResult(text)
	require.NoError(t, err)
	assert.Contains(t, body, "@@ line 3 @@")
	assert.Contains(t, body, "| two\n-three\n+THREE\n| four\n")
	require.NotNil(t, header.Edit)
	assert.Equal(t, []int{3}, header.Edit.Lines)
}

func TestEditStrategyCatalogInventory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		strategy config.EditStrategy
		edit     bool
		write    bool
	}{
		{"none", config.EditStrategyNone, false, false},
		{"replace", config.EditStrategyReplace, true, false},
		{"write_and_replace", config.EditStrategyWriteAndReplace, true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newToolFixtureWithStrategy(t, tools.DefaultLimits(), test.strategy)
			for _, name := range []string{"read", "ls", "glob", "grep"} {
				assert.Contains(t, fixture.tools, name)
			}

			_, edit := fixture.tools["Edit"]
			_, write := fixture.tools["Write"]
			assert.Equal(t, test.edit, edit)
			assert.Equal(t, test.write, write)
			assert.NotContains(t, fixture.tools, "apply_patch")
			assert.NotContains(t, fixture.tools, "search_replace")
			assert.NotContains(t, fixture.tools, "write")
		})
	}
}
