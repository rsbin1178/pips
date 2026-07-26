//nolint:wsl_v5 // Runtime setup, prompt execution, and request assertions stay grouped.
package coding

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/instructions"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/skillsettings"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCodingSystemPromptComposesDeterministicLayers(t *testing.T) {
	t.Parallel()

	prompt, err := buildCodingSystemPrompt(systemPromptOptions{
		Model:               "example/model",
		WorkingDirectory:    "/workspace",
		Platform:            "linux",
		Date:                "2026-07-25",
		Sandbox:             "workspace-write",
		Approval:            "on-request",
		WorkspaceTrusted:    true,
		Mode:                ModeAgent,
		ToolNames:           []string{"tool_search", "read", "read", "spawn_agent"},
		ProjectInstructions: "<project_instructions>Keep the boundary.</project_instructions>",
	})
	require.NoError(t, err)

	assert.Contains(t, prompt, "You are Pips, a terminal-first coding agent")
	assert.Contains(t, prompt, `"working_directory": "/workspace"`)
	assert.Contains(t, prompt, `"visible_tools": [`+"\n"+`    "read",`)
	assert.Equal(t, 1, strings.Count(prompt, `"read"`))
	assert.Contains(t, prompt, "Use tool_search only when an Extension or MCP tool is needed")
	assert.Contains(t, prompt, "Use spawn_agent for independent background work")
	assert.NotContains(t, prompt, "Use run_subagent for one bounded specialist result")
	assert.Less(t, strings.Index(prompt, "# Runtime environment"), strings.Index(prompt, "# Tool guidance"))
	assert.Less(t, strings.Index(prompt, "# Tool guidance"), strings.Index(prompt, "# Project instructions"))
}

func TestBuildCodingSystemPromptKeepsSharedPrefixStableAcrossModes(t *testing.T) {
	t.Parallel()

	base := systemPromptOptions{
		Model: "example/model", WorkingDirectory: "/workspace", Platform: "linux",
		Date: "2026-07-25", Sandbox: "workspace-write", Approval: "on-request",
		WorkspaceTrusted: true, ProjectInstructions: "Keep the boundary.",
	}
	agentOptions := base
	agentOptions.Mode = ModeAgent
	agentOptions.ToolNames = []string{"read", "shell", "apply_patch"}
	planOptions := base
	planOptions.Mode = ModePlan
	planOptions.PlanDocument = "session-bound:s-1"
	planOptions.ToolNames = []string{"read", "read_plan", "write_plan"}

	agentParts, err := buildCodingSystemPromptParts(agentOptions)
	require.NoError(t, err)
	planParts, err := buildCodingSystemPromptParts(planOptions)
	require.NoError(t, err)

	assert.Equal(t, agentParts.SharedPrefix, planParts.SharedPrefix)
	assert.NotEqual(t, agentParts.Suffix, planParts.Suffix)
	assert.NotContains(t, agentParts.SharedPrefix, "operating_mode")
	assert.NotContains(t, agentParts.SharedPrefix, "visible_tools")
	assert.Contains(t, planParts.Suffix, `"operating_mode": "plan"`)
	assert.Contains(t, planParts.Suffix, "write_plan")
}

func TestRuntimeInjectsMainSystemPromptAndProjectInstructions(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspacePath := base + "/workspace"
	require.NoError(t, mkdirPrivate(workspacePath))
	require.NoError(t, os.WriteFile(
		workspacePath+"/AGENTS.md",
		[]byte("Always preserve the system-prompt sentinel.\n"),
		0o600,
	))

	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("inspect")))
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)

	requests := model.Requests()
	require.Len(t, requests, 1)
	request := requests[0]
	assert.Contains(t, request.System, "You are Pips, a terminal-first coding agent")
	assert.Contains(t, request.System, workspacePath)
	assert.Contains(t, request.System, `source="AGENTS.md"`)
	assert.Contains(t, request.System, "Always preserve the system-prompt sentinel.")
	assert.Contains(t, request.System, `"model": "openai/runtime-test"`)
	assert.Contains(t, request.System, `"sandbox": "workspace-write"`)
	assert.Contains(t, request.System, `"approval": "on-request"`)
	assert.Contains(t, request.System, `"workspace_trusted": false`)
	assert.Contains(t, request.System, `"apply_patch"`)
	assert.Contains(t, request.System, `"run_subagent"`)
	assert.Contains(t, request.System, `"spawn_agent"`)
}

func TestRuntimeExplicitUserSkillIsInteractionScoped(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	skillDir := base + "/home/skills/review"
	require.NoError(t, os.MkdirAll(skillDir, 0o700))
	require.NoError(t, os.WriteFile(
		skillDir+"/SKILL.md",
		[]byte("---\nname: review\ndescription: Review carefully\ndisable-model-invocation: true\n---\nEXPLICIT_REVIEW_INSTRUCTIONS\n"),
		0o600,
	))

	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)

	snapshot, err := runtime.Skills(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Skills, 1)
	assert.True(t, snapshot.Skills[0].UserInvocable)
	assert.False(t, snapshot.Skills[0].ModelInvocable)
	assert.Equal(t, SkillSourceUserPips, snapshot.Skills[0].Source)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("$review inspect this")))
	requests := model.Requests()
	require.Len(t, requests, 1)
	assert.Contains(t, requests[0].System, "# Explicitly selected Skills")
	assert.Contains(t, requests[0].System, "EXPLICIT_REVIEW_INSTRUCTIONS")
	assert.NotContains(t, requests[0].System, "<available-skills>")
	assert.Contains(t, toolNamesFromRequest(requests[0]), "skill")
	assert.True(t, requestContainsText(requests[0], "$review inspect this"))
}

func TestRuntimeDoesNotExposeUnselectedUserOnlySkill(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	skillDir := base + "/home/skills/review"
	require.NoError(t, os.MkdirAll(skillDir, 0o700))
	require.NoError(t, os.WriteFile(
		skillDir+"/SKILL.md",
		[]byte("---\nname: review\ndescription: Review carefully\ndisable-model-invocation: true\n---\nPRIVATE_REVIEW_INSTRUCTIONS\n"),
		0o600,
	))

	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("inspect this")))

	requests := model.Requests()
	require.Len(t, requests, 1)
	assert.NotContains(t, requests[0].System, "PRIVATE_REVIEW_INSTRUCTIONS")
	assert.NotContains(t, requests[0].System, "<available-skills>")
	assert.NotContains(t, toolNamesFromRequest(requests[0]), "skill")
}

func TestRuntimeSkillPolicyDisablesInjectionAndPersists(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	skillDir := base + "/home/skills/review"
	require.NoError(t, os.MkdirAll(skillDir, 0o700))
	require.NoError(t, os.WriteFile(
		skillDir+"/SKILL.md",
		[]byte("---\nname: review\ndescription: Review carefully\n---\nDISABLED_REVIEW_INSTRUCTIONS\n"),
		0o600,
	))

	firstModel := newRuntimeModel(runtimeTextResponse("done"))
	first := openFullAccessTestRuntimeConfiguredWithTrust(
		t, base, SessionTarget{}, firstModel, nil, nil, nil, true,
	)
	snapshot, err := first.Skills(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Skills, 1)
	assert.NotEmpty(t, snapshot.Skills[0].ID)
	assert.True(t, snapshot.Skills[0].Enabled)

	require.NoError(t, first.SetSkillEnabled(t.Context(), snapshot.Skills[0].ID, false))
	snapshot, err = first.Skills(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Skills, 1)
	assert.False(t, snapshot.Skills[0].Enabled)

	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("$review inspect this")))
	requests := firstModel.Requests()
	require.Len(t, requests, 1)
	assert.NotContains(t, requests[0].System, "DISABLED_REVIEW_INSTRUCTIONS")
	assert.NotContains(t, requests[0].System, "<available-skills>")
	assert.NotContains(t, toolNamesFromRequest(requests[0]), "skill")
	require.NoError(t, first.Close(t.Context()))

	second := openFullAccessTestRuntimeConfiguredWithTrust(
		t,
		base,
		SessionTarget{},
		newRuntimeModel(runtimeTextResponse("done")),
		nil,
		nil,
		nil,
		true,
	)
	persisted, err := second.Skills(t.Context())
	require.NoError(t, err)
	require.Len(t, persisted.Skills, 1)
	assert.Equal(t, snapshot.Skills[0].ID, persisted.Skills[0].ID)
	assert.False(t, persisted.Skills[0].Enabled)
}

func TestRuntimeSkillDiagnosticsStayOutOfTimeline(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	skillDir := base + "/home/skills/review"
	require.NoError(t, os.MkdirAll(skillDir, 0o700))
	require.NoError(t, os.WriteFile(
		skillDir+"/SKILL.md",
		[]byte("---\nname: review\ndescription: Review carefully\nmetadata:\n  count: 2\n---\nREVIEW\n"),
		0o600,
	))

	runtime := openTestRuntimeAt(
		t,
		base,
		SessionTarget{},
		newRuntimeModel(runtimeTextResponse("done")),
	)
	snapshot, err := runtime.Skills(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.Diagnostics)

	for _, diagnostic := range runtime.Snapshot().Diagnostics {
		assert.NotEqual(t, "resource", diagnostic.Component)
	}
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("inspect")))
	for _, event := range events {
		diagnostic, ok := event.Payload.(IntegrationDiagnostic)
		if ok {
			assert.NotEqual(t, "resource", diagnostic.Component)
		}
	}
}

func TestRuntimeRejectsUntrustedStaleBusyAndFailedSkillMutations(t *testing.T) {
	t.Parallel()

	t.Run("untrusted", func(t *testing.T) {
		t.Parallel()

		base := t.TempDir()
		writeRuntimeTestSkill(t, base, "POLICY_TEST")
		runtime := openTestRuntimeAt(
			t, base, SessionTarget{}, newRuntimeModel(runtimeTextResponse("done")),
		)
		snapshot, err := runtime.Skills(t.Context())
		require.NoError(t, err)
		require.Len(t, snapshot.Skills, 1)
		require.ErrorIs(
			t,
			runtime.SetSkillEnabled(t.Context(), snapshot.Skills[0].ID, false),
			skillsettings.ErrUntrusted,
		)
	})

	t.Run("stale and busy", func(t *testing.T) {
		t.Parallel()

		base := t.TempDir()
		writeRuntimeTestSkill(t, base, "POLICY_TEST")
		runtime := openFullAccessTestRuntimeConfiguredWithTrust(
			t,
			base,
			SessionTarget{},
			newRuntimeModel(runtimeTextResponse("done")),
			nil,
			nil,
			nil,
			true,
		)
		require.ErrorIs(
			t,
			runtime.SetSkillEnabled(t.Context(), SkillID("stale"), false),
			ErrRuntimeInvalid,
		)
		snapshot, err := runtime.Skills(t.Context())
		require.NoError(t, err)
		require.Len(t, snapshot.Skills, 1)

		runtime.mu.Lock()
		runtime.state.Phase = PhaseRunning
		runtime.mu.Unlock()
		require.ErrorIs(
			t,
			runtime.SetSkillEnabled(t.Context(), snapshot.Skills[0].ID, false),
			ErrRuntimeBusy,
		)
		runtime.mu.Lock()
		runtime.state.Phase = PhaseIdle
		runtime.mu.Unlock()
	})

	t.Run("unsafe file preserves policy", func(t *testing.T) {
		t.Parallel()

		base := t.TempDir()
		writeRuntimeTestSkill(t, base, "POLICY_TEST")
		runtime := openFullAccessTestRuntimeConfiguredWithTrust(
			t,
			base,
			SessionTarget{},
			newRuntimeModel(runtimeTextResponse("done")),
			nil,
			nil,
			nil,
			true,
		)
		snapshot, err := runtime.Skills(t.Context())
		require.NoError(t, err)
		require.Len(t, snapshot.Skills, 1)
		projectDir := base + "/workspace/.pips"
		require.NoError(t, os.MkdirAll(projectDir, 0o700))
		require.NoError(t, os.WriteFile( //nolint:gosec // Broad mode is the unsafe-file test input.
			projectDir+"/skills.toml",
			[]byte("schema = \""+skillsettings.Schema+"\"\n"),
			0o644,
		))
		require.ErrorIs(
			t,
			runtime.SetSkillEnabled(t.Context(), snapshot.Skills[0].ID, false),
			skillsettings.ErrUnsafeFile,
		)
		unchanged, err := runtime.Skills(t.Context())
		require.NoError(t, err)
		assert.True(t, unchanged.Skills[0].Enabled)
	})
}

func TestRuntimeReloadPublishesProjectSkillPolicy(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	writeRuntimeTestSkill(t, base, "POLICY_TEST")
	runtime := openFullAccessTestRuntimeConfiguredWithTrust(
		t,
		base,
		SessionTarget{},
		newRuntimeModel(runtimeTextResponse("done")),
		nil,
		nil,
		nil,
		true,
	)
	snapshot, err := runtime.Skills(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Skills, 1)
	assert.True(t, snapshot.Skills[0].Enabled)

	projectDir := base + "/workspace/.pips"
	require.NoError(t, os.MkdirAll(projectDir, 0o700))
	require.NoError(t, os.WriteFile(
		projectDir+"/skills.toml",
		[]byte("schema = \""+skillsettings.Schema+"\"\n\n"+
			"[[disabled]]\nsource = \"user:pips/review/SKILL.md\"\nname = \"review\"\n"),
		0o600,
	))
	require.NoError(t, runtime.Reload(t.Context()))

	reloaded, err := runtime.Skills(t.Context())
	require.NoError(t, err)
	require.Len(t, reloaded.Skills, 1)
	assert.Equal(t, snapshot.Skills[0].ID, reloaded.Skills[0].ID)
	assert.False(t, reloaded.Skills[0].Enabled)
}

func writeRuntimeTestSkill(t *testing.T, base, content string) {
	t.Helper()

	skillDir := base + "/home/skills/review"
	require.NoError(t, os.MkdirAll(skillDir, 0o700))
	require.NoError(t, os.WriteFile(
		skillDir+"/SKILL.md",
		[]byte("---\nname: review\ndescription: Review carefully\n---\n"+content+"\n"),
		0o600,
	))
}

func TestRuntimeRejectsInvalidProjectInstructionsBeforeModelRequest(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspacePath := base + "/workspace"
	require.NoError(t, mkdirPrivate(workspacePath))
	require.NoError(t, os.WriteFile(workspacePath+"/AGENTS.md", []byte{'p', 0, 's'}, 0o600))

	model := newRuntimeModel(runtimeTextResponse("must not run"))
	openedWorkspace, err := workspace.Open(workspacePath)
	require.NoError(t, err)
	layout, err := paths.New(base + "/home")
	require.NoError(t, err)
	cfg := config.Defaults()
	cfg.Model = config.ModelRef{Provider: model.Provider(), Model: model.ModelID()}
	_, err = Open(t.Context(), OpenOptions{
		Workspace: openedWorkspace,
		Config:    cfg,
		Paths:     layout,
		Model:     model,
		Execution: ExecutionOptions{
			SandboxProbe: func(context.Context, *execution.Executor) error { return nil },
		},
	})

	require.ErrorIs(t, err, instructions.ErrBinary)
	assert.Empty(t, model.Requests())
}
