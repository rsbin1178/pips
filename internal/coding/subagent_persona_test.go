package coding

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntersectCapabilityModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		left  agentprofile.CapabilityMode
		right agentprofile.CapabilityMode
		want  agentprofile.CapabilityMode
	}{
		{"unconstrained left", "", agentprofile.CapabilityReadOnly, agentprofile.CapabilityReadOnly},
		{"unconstrained right", agentprofile.CapabilityExecute, "", agentprofile.CapabilityExecute},
		{"all is identity", agentprofile.CapabilityAll, agentprofile.CapabilityReadWrite, agentprofile.CapabilityReadWrite},
		{"read-only is bottom", agentprofile.CapabilityReadWrite, agentprofile.CapabilityReadOnly, agentprofile.CapabilityReadOnly},
		{"equal modes pass", agentprofile.CapabilityExecute, agentprofile.CapabilityExecute, agentprofile.CapabilityExecute},
		{"orthogonal modes meet at read-only", agentprofile.CapabilityReadWrite, agentprofile.CapabilityExecute, agentprofile.CapabilityReadOnly},
		{"orthogonal modes are symmetric", agentprofile.CapabilityExecute, agentprofile.CapabilityReadWrite, agentprofile.CapabilityReadOnly},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, intersectCapabilityModes(test.left, test.right))
		})
	}
}

func TestResolveChildIsolationPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested string
		role      config.SubagentRole
		persona   config.SubagentPersona
		want      string
	}{
		{"caller worktree wins", subagent.IsolationWorktree, config.SubagentRole{}, config.SubagentPersona{}, subagent.IsolationWorktree},
		{"role default applies", "", config.SubagentRole{DefaultIsolation: "worktree"}, config.SubagentPersona{}, subagent.IsolationWorktree},
		{"persona default applies", "", config.SubagentRole{}, config.SubagentPersona{DefaultIsolation: "worktree"}, subagent.IsolationWorktree},
		{"role tier outranks persona tier", "", config.SubagentRole{DefaultIsolation: "worktree"}, config.SubagentPersona{DefaultIsolation: "none"}, subagent.IsolationWorktree},
		{"caller none keeps the definition default", "none", config.SubagentRole{DefaultIsolation: "worktree"}, config.SubagentPersona{}, subagent.IsolationWorktree},
		{"no preset selects the shared workspace", "", config.SubagentRole{DefaultIsolation: "none"}, config.SubagentPersona{}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, resolveChildIsolation(test.requested, test.role, test.persona))
		})
	}
}

func TestRuntimeAppliesRoleAndPersonaToBuiltinChild(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspacePath := filepath.Join(base, "workspace")
	require.NoError(t, mkdirPrivate(workspacePath))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspacePath, "role-notes.md"),
		[]byte("Follow the house review checklist.\n"),
		0o600,
	))

	model := newReasoningRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName,
			`{"prompt":"Inspect the tree.","description":"delegate","run_in_background":false}`),
		runtimeTextResponse("child summary"),
		runtimeTextResponse("delegation complete"),
	)
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, SessionTarget{}, model, nil, nil, nil, false, config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.Subagent.Roles = map[string]config.SubagentRole{
				"general-purpose": {
					Description:           "Built-in house style",
					DefaultCapabilityMode: "read-only",
					PromptFile:            "role-notes.md",
					ReasoningEffort:       "high",
				},
			}
			cfg.Subagent.Personas = map[string]config.SubagentPersona{
				"general-purpose": {Instructions: "Persona guidance."},
			}
			cfg.Subagent.Models = map[string]string{"general-purpose": "openai/not-configured"}
		},
	)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)
	// An unusable model tier is reported through the runtime diagnostics rather
	// than silently dropped.
	diagnosticCodes := make([]string, 0)
	for _, diagnostic := range runtime.Snapshot().Diagnostics {
		diagnosticCodes = append(diagnosticCodes, diagnostic.Code)
	}

	assert.Contains(t, diagnosticCodes, "subagent_model_fallback")

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	detail, err := runtime.InspectSubagent(t.Context(), summaries[0].ChildSessionID)
	require.NoError(t, err)
	// The per-type override cannot be resolved, so the child runs the parent
	// model.
	assert.Equal(t, "openai/runtime-test", detail.Plan.Model)
	assert.Equal(t, "general-purpose", detail.Plan.Persona)
	assert.Equal(t, "Persona guidance.", detail.Plan.PersonaReminder)
	assert.Equal(t, "high", detail.Plan.ReasoningEffort)
	assert.Contains(t, detail.Plan.Instructions, "<role-instructions>")
	assert.Contains(t, detail.Plan.Instructions, "Follow the house review checklist.")

	// The role's read-only mode intersects the built-in's full mode.
	capabilities := make([]string, 0, len(detail.Plan.Capabilities))
	for _, capability := range detail.Plan.Capabilities {
		capabilities = append(capabilities, capability.WireName)
	}

	assert.Contains(t, capabilities, "read")
	assert.NotContains(t, capabilities, "Edit")
	assert.NotContains(t, capabilities, "shell")

	// The persona reminder is a persisted synthetic user item before the task,
	// and it is inserted exactly once.
	texts := personaTranscriptTexts(detail.Transcript)
	assert.Equal(t, 1, countText(texts, "<system-reminder>\nPersona guidance.\n</system-reminder>"))
	assert.Contains(t, texts, "Inspect the tree.")

	// The reasoning level reaches the wire for a reasoning-capable model.
	requests := model.Requests()
	require.GreaterOrEqual(t, len(requests), 2)
	require.NotNil(t, requests[1].Reasoning)
	assert.Equal(t, ai.ReasoningHigh, requests[1].Reasoning.Effort)
}

func TestRuntimeDiscoversUserRolePresetFile(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.RolesDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.RolesDir(), "general-purpose.toml"),
		[]byte("description = \"User preset\"\nprompt_file = \"preset.md\"\n"),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.RolesDir(), "preset.md"),
		[]byte("User preset instructions.\n"),
		0o600,
	))

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName,
			`{"prompt":"Inspect.","description":"delegate","run_in_background":false}`),
		runtimeTextResponse("child summary"),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	detail, err := runtime.InspectSubagent(t.Context(), summaries[0].ChildSessionID)
	require.NoError(t, err)
	assert.Contains(t, detail.Plan.Instructions, "User preset instructions.")
	assert.Empty(t, detail.Plan.Persona)
	assert.Empty(t, detail.Plan.PersonaReminder)
}

func TestMessagesCarryPersonaReminder(t *testing.T) {
	t.Parallel()

	reminder := "Persona guidance."
	reminderMessage := ai.UserText(personaReminderText(reminder))

	assert.False(t, messagesCarryReminder(nil, reminder))
	assert.False(t, messagesCarryReminder([]ai.Message{ai.UserText("do the task")}, reminder))
	assert.True(t, messagesCarryReminder(
		[]ai.Message{reminderMessage, ai.UserText("do the task")}, reminder,
	))
	// A different persona's reminder must not match.
	assert.False(t, messagesCarryReminder([]ai.Message{reminderMessage}, "Other guidance."))
}

// reasoningRuntimeModel reports reasoning support so the child request policy
// can be observed on the wire.
type reasoningRuntimeModel struct{ *runtimeModel }

func newReasoningRuntimeModel(responses ...*ai.Response) reasoningRuntimeModel {
	return reasoningRuntimeModel{newRuntimeModel(responses...)}
}

func (m reasoningRuntimeModel) Capabilities() ai.Capabilities {
	capabilities := m.runtimeModel.Capabilities()
	capabilities.Reasoning = true

	return capabilities
}

func countText(values []string, want string) int {
	count := 0

	for _, value := range values {
		if value == want {
			count++
		}
	}

	return count
}

func personaTranscriptTexts(messages []ai.Message) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}

		for _, part := range parts {
			if text, ok := part.(ai.TextPart); ok {
				texts = append(texts, text.Text)
			}
		}
	}

	return texts
}
