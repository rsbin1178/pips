//nolint:gosec // Explicit CLI fixture paths are test-only.
package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestAgentsGenerateRequiresExplicitReviewBeforePromotion(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	runtime := &fakeAgentRunRuntime{
		draft: coding.AgentDraft{
			Summary: coding.AgentDraftSummary{
				DraftID: "draft-review", AgentID: "release-review",
				Scope: coding.AgentDraftScopeUser, DefinitionDigest: "digest-review",
			},
			Definition: []byte("PRIVATE_REVIEW_DEFINITION"),
		},
		promotion: coding.AgentDraftPromotion{
			AgentID: "release-review", Scope: coding.AgentDraftScopeUser,
			DefinitionDigest: "digest-review", Target: "/private/agents/release-review.md",
			GenerationID: 7, ReloadRequired: true,
		},
	}
	dependencies := fixture.dependencies(map[string]string{config.ModelEnv: "openai/test-model"})
	dependencies.OpenAgentRun = func(
		_ context.Context,
		options coding.OpenOptions,
	) (cli.AgentRunRuntime, error) {
		runtime.options = options

		return runtime, nil
	}

	output, err := executeAgentsWithInput(
		t,
		dependencies,
		"promote\n",
		"--dynamic-subagents",
		"agents",
		"generate",
		"release-review",
		"Review release changes",
	)
	require.NoError(t, err)
	assert.Contains(t, output, `"schema":"pips.coding.agent.draft/v1alpha1"`)
	assert.Contains(t, output, `"definition":"PRIVATE_REVIEW_DEFINITION"`)
	assert.Contains(t, output, "Review the draft above")
	assert.Contains(t, output, `"schema":"pips.coding.agent.draft.promotion/v1alpha1"`)
	assert.Contains(t, output, `"reload_required":true`)
	assert.Equal(t, "release-review", runtime.generate.AgentID)
	assert.Equal(t, "Review release changes", runtime.generate.Intent)
	assert.Equal(t, "draft-review", runtime.promote.DraftID)
	assert.Equal(t, "digest-review", runtime.promote.ExpectedDigest)
	assert.False(t, runtime.discarded)
	assert.True(t, runtime.options.Config.DynamicSubagents)
	assert.True(t, runtime.closed)
}

func TestAgentsGenerateDefaultsToDiscardOnEOF(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	runtime := &fakeAgentRunRuntime{draft: coding.AgentDraft{
		Summary: coding.AgentDraftSummary{
			DraftID: "draft-discard", AgentID: "discard-review",
			Scope: coding.AgentDraftScopeUser, DefinitionDigest: "digest-discard",
		},
		Definition: []byte("discard definition"),
	}}
	dependencies := fixture.dependencies(map[string]string{config.ModelEnv: "openai/test-model"})
	dependencies.OpenAgentRun = func(
		context.Context,
		coding.OpenOptions,
	) (cli.AgentRunRuntime, error) {
		return runtime, nil
	}

	output, err := executeAgentsWithInput(
		t,
		dependencies,
		"",
		"--dynamic-subagents",
		"agents",
		"generate",
		"discard-review",
		"Generate then discard",
	)
	require.NoError(t, err)
	assert.Contains(t, output, `"disposition":"discarded"`)
	assert.True(t, runtime.discarded)
	assert.Empty(t, runtime.promote.DraftID)
}

func TestAgentsGenerateRevalidatesExplicitEditedDefinition(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	writeCLIFile(t, filepath.Join(fixture.workspaceDir, "edited.md"), "edited definition")

	runtime := &fakeAgentRunRuntime{
		draft: coding.AgentDraft{
			Summary: coding.AgentDraftSummary{
				DraftID: "draft-edit", AgentID: "edit-review",
				Scope: coding.AgentDraftScopeUser, DefinitionDigest: "digest-edit",
			},
			Definition: []byte("original definition"),
		},
		promotion: coding.AgentDraftPromotion{AgentID: "edit-review"},
	}
	dependencies := fixture.dependencies(map[string]string{config.ModelEnv: "openai/test-model"})
	dependencies.OpenAgentRun = func(
		context.Context,
		coding.OpenOptions,
	) (cli.AgentRunRuntime, error) {
		return runtime, nil
	}

	_, err := executeAgentsWithInput(
		t,
		dependencies,
		"edit edited.md\n",
		"--dynamic-subagents",
		"agents",
		"generate",
		"edit-review",
		"Generate then edit",
	)
	require.NoError(t, err)
	assert.Equal(t, "edited definition", string(runtime.promote.Definition))
	assert.False(t, runtime.discarded)
}

type fakeAgentRunRuntime struct {
	options   coding.OpenOptions
	request   coding.AgentRunRequest
	oneShot   coding.OneShotAgentRunRequest
	generate  coding.GenerateAgentDraftRequest
	promote   coding.PromoteAgentDraftRequest
	draft     coding.AgentDraft
	promotion coding.AgentDraftPromotion
	result    subagent.Result
	runErr    error
	discarded bool
	closed    bool
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

func (r *fakeAgentRunRuntime) GenerateAgentDraft(
	_ context.Context,
	request coding.GenerateAgentDraftRequest,
) (coding.AgentDraft, error) {
	r.generate = request

	return r.draft, nil
}

func (r *fakeAgentRunRuntime) PromoteAgentDraft(
	_ context.Context,
	request coding.PromoteAgentDraftRequest,
) (coding.AgentDraftPromotion, error) {
	r.promote = request

	return r.promotion, nil
}

func (r *fakeAgentRunRuntime) DiscardAgentDraft(
	context.Context,
	string,
	string,
) error {
	r.discarded = true

	return nil
}

func executeAgentsWithInput(
	t *testing.T,
	dependencies cli.Dependencies,
	input string,
	arguments ...string,
) (string, error) {
	t.Helper()

	command, err := cli.New(dependencies)
	require.NoError(t, err)

	buffer := new(bytes.Buffer)

	command.SetIn(strings.NewReader(input))
	command.SetOut(buffer)
	command.SetErr(buffer)
	command.SetArgs(arguments)
	err = command.ExecuteContext(t.Context())

	return buffer.String(), err
}
