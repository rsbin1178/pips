package subagent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeCwdStripsQuotesAndSentinels(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"plain", "/tmp/work", "/tmp/work"},
		{"padded", "  /tmp/work\t", "/tmp/work"},
		{"matched double quotes", `"/tmp/work"`, "/tmp/work"},
		{"matched single quotes", "'/tmp/work'", "/tmp/work"},
		{"matched backticks", "`/tmp/work`", "/tmp/work"},
		{"unmatched leading quote", `"/tmp/work`, "/tmp/work"},
		{"unmatched trailing quote", `/tmp/work"`, "/tmp/work"},
		{"quoted and padded", `"  /tmp/work  "`, "/tmp/work"},
		{"inner quote preserved", `/tmp/a"b`, `/tmp/a"b`},
		{"empty", "", ""},
		{"blank", "   ", ""},
		{"null", "null", ""},
		{"none is case insensitive", "NoNe", ""},
		{"undefined", "undefined", ""},
		{"quoted sentinel", `"null"`, ""},
		{"tilde prefix", "~/work", filepath.Join(home, "work")},
		{"bare tilde", "~", home},
		{"tilde without separator", "~other", "~other"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, SanitizeCwd(test.value))
		})
	}
}

func TestNormalizeIsolationSpellings(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "none", "None", " NONE ", "worktree", "Worktree", " WORKTREE "} {
		canonical, ok := NormalizeIsolation(value)
		require.True(t, ok, value)
		assert.Contains(t, []string{"", IsolationWorktree}, canonical, value)
	}

	for _, value := range []string{"container", "shared", "work tree"} {
		_, ok := NormalizeIsolation(value)
		assert.False(t, ok, value)
	}
}

func TestResolveCwdContracts(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	file := filepath.Join(directory, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	missing := filepath.Join(directory, "missing")

	// A padded directory is accepted and returned absolute.
	cwd, err := resolveCwd(toolArgs{Cwd: "  " + directory + "  "}, "")
	require.NoError(t, err)
	assert.Equal(t, directory, cwd)

	for name, value := range map[string]string{"file": file, "missing": missing} {
		_, err = resolveCwd(toolArgs{Cwd: value}, "")
		require.ErrorIs(t, err, ErrInvalid, name)
	}

	_, err = resolveCwd(toolArgs{Cwd: file}, "")
	assert.Contains(t, err.Error(), "exists but is not a directory")
	_, err = resolveCwd(toolArgs{Cwd: missing}, "")
	assert.Contains(t, err.Error(), "does not exist")

	// Worktree isolation and a real directory are mutually exclusive.
	_, err = resolveCwd(toolArgs{Cwd: directory}, IsolationWorktree)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "mutually exclusive")

	// A path that does not exist alongside worktree isolation is cleared so the
	// worktree wins.
	cwd, err = resolveCwd(toolArgs{Cwd: missing}, IsolationWorktree)
	require.NoError(t, err)
	assert.Empty(t, cwd)

	// A resumed child inherits its source directory, so its own cwd is not
	// validated and the resume source is what governs.
	cwd, err = resolveCwd(toolArgs{Cwd: missing, ResumeFrom: "s-source"}, "")
	require.NoError(t, err)
	assert.Equal(t, missing, cwd)
}

func TestResolveIsolationTreatsSentinelsAsAbsent(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "null", "none", "undefined", "  "} {
		isolation, err := resolveIsolation(toolArgs{Isolation: value})
		require.NoError(t, err, value)
		assert.Empty(t, isolation, value)
	}

	isolation, err := resolveIsolation(toolArgs{Isolation: "Worktree"})
	require.NoError(t, err)
	assert.Equal(t, IsolationWorktree, isolation)

	_, err = resolveIsolation(toolArgs{Isolation: "container"})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "unknown isolation")
}

func TestExecutionPlanWorkspaceValidation(t *testing.T) {
	t.Parallel()

	plan := testWorkspaceExecutionPlan(t)
	require.NoError(t, ValidateExecutionPlan(plan))

	for name, mutate := range map[string]func(*ExecutionPlan){
		"unknown isolation": func(value *ExecutionPlan) { value.Isolation = "container" },
		"worktree with cwd": func(value *ExecutionPlan) {
			value.Isolation = IsolationWorktree
			value.Cwd = "/tmp/workspace"
		},
		"relative cwd":      func(value *ExecutionPlan) { value.Cwd = "relative/path" },
		"unclean cwd":       func(value *ExecutionPlan) { value.Cwd = "/tmp/../tmp" },
		"invalid resume id": func(value *ExecutionPlan) { value.ResumeFrom = "s- with space" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cloned := plan.Clone()
			mutate(&cloned)
			assert.ErrorIs(t, ValidateExecutionPlan(cloned), ErrInvalid)
		})
	}

	for name, mutate := range map[string]func(*ExecutionPlan){
		"worktree": func(value *ExecutionPlan) { value.Isolation = IsolationWorktree },
		"cwd":      func(value *ExecutionPlan) { value.Cwd = "/tmp/workspace" },
		"resume":   func(value *ExecutionPlan) { value.ResumeFrom = "s-0123456789abcdef" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cloned := plan.Clone()
			mutate(&cloned)
			assert.NoError(t, ValidateExecutionPlan(cloned))
		})
	}
}

func TestExecutionPlanRejectsWorkspaceOnLegacyPlan(t *testing.T) {
	t.Parallel()

	plan := testWorkspaceExecutionPlan(t)
	plan.Legacy = true
	plan.Instructions = ""
	plan.InstructionsDigest = ""
	plan.MaxDelegationDepth = 0
	plan.DelegationDepth = 0
	plan.Ancestry = nil
	plan.DelegationTargets = nil
	require.NoError(t, validateExecutionPlan(plan, true))

	plan.Isolation = IsolationWorktree
	assert.ErrorIs(t, validateExecutionPlan(plan, true), ErrInvalid)
}

// testWorkspaceExecutionPlan returns one valid non-legacy plan to mutate.
func testWorkspaceExecutionPlan(t *testing.T) ExecutionPlan {
	t.Helper()

	identity, err := BuiltinIdentity(RoleExplore)
	require.NoError(t, err)
	output, err := NewOutputContract(OutputFormatText, "subagent-explore-result", nil)
	require.NoError(t, err)

	instructions := "Inspect the workspace."

	return ExecutionPlan{
		Schema:             ExecutionPlanSchema,
		Identity:           identity,
		Delivery:           DeliveryForeground,
		Model:              "openai/gpt",
		Instructions:       instructions,
		InstructionsDigest: InstructionsDigest(instructions),
		Limits:             NormalizeLimits(ProductionLimits()),
		Output:             output,
	}
}
