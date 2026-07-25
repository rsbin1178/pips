package coding

import (
	"os"
	"strings"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/instructions"
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

func TestRuntimeRejectsInvalidProjectInstructionsBeforeModelRequest(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspacePath := base + "/workspace"
	require.NoError(t, mkdirPrivate(workspacePath))
	require.NoError(t, os.WriteFile(workspacePath+"/AGENTS.md", []byte{'p', 0, 's'}, 0o600))

	model := newRuntimeModel(runtimeTextResponse("must not run"))
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)

	var runErr error

	for _, err := range runtime.Prompt(t.Context(), ai.UserText("inspect")) {
		if err != nil {
			runErr = err
		}
	}

	require.ErrorIs(t, runErr, instructions.ErrBinary)
	assert.Empty(t, model.Requests())
}
