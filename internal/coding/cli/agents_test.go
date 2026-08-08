//nolint:gosec // Explicit CLI fixture paths are test-only.
package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/cli"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentsLibraryCommandsAndExplicitTemplateCreation(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, filepath.Join(fixture.layout.AgentsDir(), "go-helper.md"), `---
schema: pips.agent/v1alpha1
name: Go helper
description: Review one bounded Go change.
tools:
  allow: ["tool:read"]
output:
  format: text
---
Read the requested files and report evidence.
`)

	output, err := executeWithDependencies(t, fixture.dependencies(nil), "agents", "list")
	require.NoError(t, err)
	assert.Contains(t, output, "go-helper\tcustom\tuser:pips\tReview one bounded Go change.")
	assert.Contains(t, output, "explore\tbuiltin\t"+"builtin")

	output, err = executeWithDependencies(t, fixture.dependencies(nil), "agents", "show", "go-helper")
	require.NoError(t, err)
	assert.Contains(t, output, "id = \"go-helper\"")
	assert.Contains(t, output, "source = \"user:pips/go-helper.md\"")
	assert.Contains(t, output, "tools.allow = \"tool:read\"")
	assert.NotContains(t, output, "Read the requested files")

	output, err = executeWithDependencies(t, fixture.dependencies(nil), "agents", "validate")
	require.NoError(t, err)
	assert.Equal(t, "Agent definitions valid\n", output)

	output, err = executeWithDependencies(t, fixture.dependencies(nil), "agents", "init", "release-review")
	require.NoError(t, err)

	target := filepath.Join(fixture.layout.AgentsDir(), "release-review.md")
	assert.Equal(t, target+"\n", output)
	data, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "schema: pips.agent/v1alpha1")

	_, err = executeWithDependencies(t, fixture.dependencies(nil), "agents", "init", "release-review")
	require.ErrorIs(t, err, cli.ErrUsage)

	writeCLIFile(t, filepath.Join(fixture.layout.AgentsDir(), "invalid.md"), "not a definition")
	output, err = executeWithDependencies(t, fixture.dependencies(nil), "agents", "validate")
	require.ErrorIs(t, err, cli.ErrUsage)
	assert.Contains(t, output, "invalid\tuser:pips/invalid.md\tmissing_frontmatter")
}

func TestAgentsRunUsesExplicitAgentRuntimeAndPrintsBoundedEnvelope(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	runtime := &fakeAgentRunRuntime{result: subagent.Result{
		Identity:       subagent.AgentIdentity{ID: "go-checker"},
		ChildSessionID: "s-child",
		Outcome:        subagent.OutcomeSucceeded,
		Value:          map[string]any{"summary": "checked"},
	}}
	dependencies := fixture.dependencies(map[string]string{config.ModelEnv: "openai/test-model"})
	dependencies.OpenAgentRun = func(
		_ context.Context,
		options coding.OpenOptions,
	) (cli.AgentRunRuntime, error) {
		runtime.options = options

		return runtime, nil
	}

	output, err := executeWithDependencies(
		t,
		dependencies,
		"--dynamic-subagents",
		"agents",
		"run",
		"go-checker",
		"inspect target.txt",
	)
	require.NoError(t, err)
	assert.Contains(t, output, `"schema":"pips.coding.agent.run/v1alpha1"`)
	assert.Contains(t, output, `"agent_id":"go-checker"`)
	assert.Contains(t, output, `"summary":"checked"`)
	assert.Equal(t, "go-checker", runtime.request.AgentID)
	assert.Equal(t, "inspect target.txt", runtime.request.Task)
	assert.True(t, runtime.options.Config.DynamicSubagents)
	assert.True(t, runtime.closed)
}

func TestAgentsRunDefinitionUsesNonPersistentRuntimePath(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	definitionPath := filepath.Join(fixture.workspaceDir, "adhoc.md")
	writeCLIFile(t, definitionPath, "one-shot definition")

	runtime := &fakeAgentRunRuntime{result: subagent.Result{
		Identity:       subagent.AgentIdentity{ID: "adhoc-checker"},
		ChildSessionID: "s-child",
		Outcome:        subagent.OutcomeSucceeded,
	}}
	dependencies := fixture.dependencies(map[string]string{config.ModelEnv: "openai/test-model"})
	dependencies.OpenAgentRun = func(context.Context, coding.OpenOptions) (cli.AgentRunRuntime, error) {
		return runtime, nil
	}

	_, err := executeWithDependencies(
		t,
		dependencies,
		"agents",
		"run",
		"--definition",
		"adhoc.md",
		"adhoc-checker",
		"inspect once",
	)
	require.NoError(t, err)
	assert.Empty(t, runtime.request.AgentID)
	assert.Equal(t, "adhoc-checker", runtime.oneShot.AgentID)
	assert.Equal(t, "inspect once", runtime.oneShot.Task)
	assert.Equal(t, "one-shot definition", string(runtime.oneShot.Definition))
}

func TestAgentsRunPropagatesNonInteractiveChildInputError(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	runtime := &fakeAgentRunRuntime{runErr: coding.ErrInputRequired}
	dependencies := fixture.dependencies(map[string]string{config.ModelEnv: "openai/test-model"})
	dependencies.OpenAgentRun = func(context.Context, coding.OpenOptions) (cli.AgentRunRuntime, error) {
		return runtime, nil
	}

	_, err := executeWithDependencies(
		t,
		dependencies,
		"--dynamic-subagents",
		"agents",
		"run",
		"decision-checker",
		"ask for the framework",
	)
	require.ErrorIs(t, err, coding.ErrInputRequired)
	assert.Equal(t, cli.ExitInput, cli.ExitCode(err))
	assert.True(t, runtime.closed)
}

type fakeAgentRunRuntime struct {
	options coding.OpenOptions
	request coding.AgentRunRequest
	oneShot coding.OneShotAgentRunRequest
	result  subagent.Result
	runErr  error
	closed  bool
}

func (r *fakeAgentRunRuntime) RunAgent(
	_ context.Context,
	request coding.AgentRunRequest,
) (subagent.Result, error) {
	r.request = request

	return r.result, r.runErr
}

func (r *fakeAgentRunRuntime) RunOneShotAgent(
	_ context.Context,
	request coding.OneShotAgentRunRequest,
) (subagent.Result, error) {
	r.oneShot = request

	return r.result, nil
}

func (r *fakeAgentRunRuntime) Close(context.Context) error {
	r.closed = true

	return nil
}
