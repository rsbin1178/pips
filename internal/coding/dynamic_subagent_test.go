//nolint:wsl_v5,paralleltest,gosec // Temporal child-control fixtures and bounded test paths stay explicit.
package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/agentprofile"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeDispatchesCustomProfileThroughChildScopedCatalog(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "go-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Go checker
description: Read one file and return a structured verification summary.
model: openai/runtime-test
tools:
  allow: ["tool:read"]
output:
  format: json_schema
  schema:
    type: object
    additionalProperties: false
    required: [summary, verification]
    properties:
      summary: {type: string, maxLength: 4000}
      verification:
        type: array
        maxItems: 32
        items: {type: string, maxLength: 1000}
---
Read only the delegated target and report the verification evidence.
`),
		0o600,
	))

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName, `{"agent_id":"go-checker","task":"Inspect target.txt."}`),
		runtimeToolResponse("child-read", "read", `{"path":"target.txt"}`),
		runtimeTextResponse(`{"summary":"target read","verification":["target.txt"]}`),
		runtimeTextResponse("delegation complete"),
	)
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)
	require.NoError(t, os.WriteFile(
		filepath.Join(runtime.workspace.Root(), "target.txt"), []byte("verified\n"), 0o600,
	))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate the check")))
	assert.Contains(t, eventTypes(events), EventSubagentCreated)
	assert.Contains(t, eventTypes(events), EventSubagentCompleted)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	summary := summaries[0]
	assert.Equal(t, subagent.AgentKindCustom, summary.Identity.Kind)
	assert.Equal(t, "go-checker", summary.Identity.ID)
	assert.Empty(t, summary.Role)
	assert.Equal(t, subagent.StateSucceeded, summary.State)

	detail, err := runtime.InspectSubagent(t.Context(), summary.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, "go-checker", detail.Plan.Identity.ID)
	assert.Equal(t, "openai/runtime-test", detail.Plan.Model)
	assert.Equal(t, []subagent.EffectiveCapability{{
		WireName: "read", Source: string(catalog.SourceLocal) + "/coding", Risk: "read",
	}}, detail.Plan.Capabilities)
	result, ok := detail.Result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "target read", result["summary"])
	assert.Equal(t, 1, summary.ToolCalls)

	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.Equal(t, []string{"read"}, toolNamesFromRequest(requests[1]))
	assert.NotContains(t, toolNamesFromRequest(requests[1]), subagent.ToolName)
	assert.Nil(t, requests[1].ResponseFormat)
}

func TestRuntimeDispatchesBoundedExactTargetRecursiveChain(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	definitions := map[string]string{
		"root-agent.md": `---
schema: pips.agent/v1alpha1
name: Root agent
description: Delegate one exact middle task.
delegation:
  allow: [middle-agent]
---
Delegate the task to middle-agent and return its result.
`,
		"middle-agent.md": `---
schema: pips.agent/v1alpha1
name: Middle agent
description: Delegate one exact leaf task.
visibility:
  user: false
  model: true
delegation:
  allow: [leaf-agent]
---
Delegate the task to leaf-agent and return its result.
`,
		"leaf-agent.md": `---
schema: pips.agent/v1alpha1
name: Leaf agent
description: Return one bounded leaf result.
visibility:
  user: false
  model: true
---
Return the leaf result without tools.
`,
	}
	for name, body := range definitions {
		require.NoError(t, os.WriteFile(filepath.Join(layout.AgentsDir(), name), []byte(body), 0o600))
	}

	model := newRuntimeModel(
		runtimeToolResponse("parent-root", subagent.ToolName, `{"agent_id":"root-agent","task":"Complete the chain."}`),
		runtimeToolResponse("root-middle", subagent.ToolName, `{"agent_id":"middle-agent","task":"Delegate to the leaf."}`),
		runtimeToolResponse("middle-leaf", subagent.ToolName, `{"agent_id":"leaf-agent","task":"Return the leaf evidence."}`),
		runtimeTextResponse("leaf evidence"),
		runtimeTextResponse("middle received leaf evidence"),
		runtimeTextResponse("root received middle evidence"),
		runtimeTextResponse("recursive delegation complete"),
	)
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.DynamicSubagents = true
			cfg.Subagent.MaxDepth = 2
		},
	)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("run the recursive chain")))
	assert.Contains(t, eventTypes(events), EventSubagentCompleted)
	requests := model.Requests()
	require.Len(t, requests, 7)
	assert.Equal(t, []string{subagent.ToolName}, toolNamesFromRequest(requests[1]))
	assert.Equal(t, []string{subagent.ToolName}, toolNamesFromRequest(requests[2]))
	assert.Empty(t, toolNamesFromRequest(requests[3]))
	assert.NotContains(t, toolNamesFromRequest(requests[1]), subagent.SpawnToolName)
	assert.Equal(t, []any{"middle-agent"}, requests[1].Tools[0].InputSchema.Properties["agent_id"].Enum)
	assert.Equal(t, []any{"leaf-agent"}, requests[2].Tools[0].InputSchema.Properties["agent_id"].Enum)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 3)
	byID := make(map[string]subagent.Summary, len(summaries))
	for _, summary := range summaries {
		byID[summary.Identity.ID] = summary
	}
	rootSummary := byID["root-agent"]
	middleSummary := byID["middle-agent"]
	leafSummary := byID["leaf-agent"]
	assert.Equal(t, runtime.handle.Metadata().ID, rootSummary.Ownership.ParentSessionID)
	assert.Equal(t, rootSummary.ChildSessionID, middleSummary.Ownership.ParentSessionID)
	assert.Equal(t, middleSummary.ChildSessionID, leafSummary.Ownership.ParentSessionID)
	assert.NotEmpty(t, middleSummary.Ownership.ParentRunID)
	assert.NotEmpty(t, leafSummary.Ownership.ParentRunID)

	var generationID uint64
	for id, want := range map[string]struct {
		depth    int
		ancestry []string
		targets  []string
	}{
		"root-agent":   {depth: 0, ancestry: []string{"root-agent"}, targets: []string{"middle-agent"}},
		"middle-agent": {depth: 1, ancestry: []string{"root-agent", "middle-agent"}, targets: []string{"leaf-agent"}},
		"leaf-agent":   {depth: 2, ancestry: []string{"root-agent", "middle-agent", "leaf-agent"}},
	} {
		detail, inspectErr := runtime.InspectSubagent(t.Context(), byID[id].ChildSessionID)
		require.NoError(t, inspectErr, id)
		if generationID == 0 {
			generationID = detail.Plan.GenerationID
		}
		assert.Equal(t, generationID, detail.Plan.GenerationID, id)
		assert.Equal(t, want.depth, detail.Plan.DelegationDepth, id)
		assert.Equal(t, 2, detail.Plan.MaxDelegationDepth, id)
		assert.Equal(t, want.ancestry, detail.Plan.Ancestry, id)
		assert.Equal(t, want.targets, detail.Plan.DelegationTargets, id)
		assert.Equal(t, rootSummary.Identity.DefinitionSchema, detail.Plan.Identity.DefinitionSchema)
	}
	leafResult, err := runtime.WaitSubagent(t.Context(), leafSummary.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, "leaf evidence", leafResult.Value)
	require.NoError(t, runtime.CancelSubagent(t.Context(), leafSummary.ChildSessionID))
}

func TestRuntimeRecursiveAdmissionRejectsBeforeCreatingAThirdChild(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*config.SubagentConfig)
		wantText string
	}{
		{
			name: "shared concurrent slots",
			mutate: func(value *config.SubagentConfig) {
				value.MaxDepth = 2
				value.MaxConcurrent = 2
			},
			wantText: "capacity exhausted",
		},
		{
			name: "shared cumulative descendants",
			mutate: func(value *config.SubagentConfig) {
				value.MaxDepth = 2
				value.MaxSpawnedPerRootInteraction = 1
			},
			wantText: "spawn limit exhausted",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			layout, err := paths.New(filepath.Join(base, "home"))
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
			for name, body := range map[string]string{
				"root-agent.md": `---
schema: pips.agent/v1alpha1
name: Root agent
description: Delegate one middle task.
delegation:
  allow: [middle-agent]
---
Delegate to middle-agent.
`,
				"middle-agent.md": `---
schema: pips.agent/v1alpha1
name: Middle agent
description: Delegate one leaf task.
delegation:
  allow: [leaf-agent]
---
Delegate to leaf-agent.
`,
				"leaf-agent.md": `---
schema: pips.agent/v1alpha1
name: Leaf agent
description: Return the leaf result.
---
Return the leaf result.
`,
			} {
				require.NoError(t, os.WriteFile(filepath.Join(layout.AgentsDir(), name), []byte(body), 0o600))
			}
			model := newRuntimeModel(
				runtimeToolResponse("parent-root", subagent.ToolName, `{"agent_id":"root-agent","task":"Start."}`),
				runtimeToolResponse("root-middle", subagent.ToolName, `{"agent_id":"middle-agent","task":"Continue."}`),
				runtimeToolResponse("middle-leaf", subagent.ToolName, `{"agent_id":"leaf-agent","task":"Finish."}`),
				runtimeTextResponse("middle handled admission rejection"),
				runtimeTextResponse("root handled child result"),
				runtimeTextResponse("parent complete"),
			)
			runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
				t, base, SessionTarget{}, model, nil, nil, nil, false,
				config.SandboxWorkspaceWrite,
				func(cfg *config.Config) {
					cfg.DynamicSubagents = true
					test.mutate(&cfg.Subagent)
				},
			)
			collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("exercise recursive admission")))

			requests := model.Requests()
			require.Len(t, requests, 6)
			assert.True(t, requestContainsToolText(requests[3], test.wantText))
			summaries, err := runtime.ListSubagents(t.Context())
			require.NoError(t, err)
			require.Len(t, summaries, 2)
			for _, summary := range summaries {
				assert.NotEqual(t, "leaf-agent", summary.Identity.ID)
			}
		})
	}
}

func TestRuntimeCustomProfileDispatchRequiresAlphaGate(t *testing.T) {
	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "go-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Go checker
description: Check one bounded target.
tools:
  allow: ["tool:read"]
output:
  format: text
---
Inspect the delegated target.
`),
		0o600,
	))

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName, `{"agent_id":"go-checker","task":"Inspect target."}`),
		runtimeTextResponse("custom delegation is unavailable"),
	)
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate the check")))
	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	assert.Empty(t, summaries)

	requests := model.Requests()
	require.Len(t, requests, 2)
	for _, tool := range requests[0].Tools {
		if tool.Name != subagent.ToolName {
			continue
		}
		require.NotNil(t, tool.InputSchema)
		assert.NotContains(t, tool.InputSchema.Properties["agent_id"].Enum, "go-checker")
		return
	}
	require.Fail(t, "run_subagent tool was not declared")
}

func TestRuntimeListAgentProfilesIsContentSafeAndReportsAvailability(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "go-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Go checker
description: Inspect one bounded Go target.
model: inherit
visibility:
  user: true
  model: true
delivery: [foreground]
tools:
  allow: ["tool:read"]
  require: ["tool:read"]
output:
  format: text
---
PRIVATE INSTRUCTIONS MUST NOT APPEAR IN THE LIBRARY.
`),
		0o600,
	))

	runtime := openTestRuntimeAt(t, base, SessionTarget{}, newRuntimeModel(runtimeTextResponse("unused")))
	library, err := runtime.ListAgentProfiles(t.Context())
	require.NoError(t, err)
	require.NoError(t, ValidateAgentLibrary(library))

	profile, found := agentLibraryEntry(library, "go-checker")
	require.True(t, found)
	assert.False(t, profile.Available)
	assert.Equal(t, "dynamic subagents is disabled", profile.Unavailable)
	assert.Equal(t, []string{"tool:read"}, profile.DeclaredTools)
	assert.Equal(t, []string{"tool:read"}, profile.RequiredTools)
	assert.Equal(t, "user:pips/go-checker.md", profile.Source)
	assert.NotContains(t, fmt.Sprintf("%+v", profile), "PRIVATE INSTRUCTIONS")

	runtime.config.DynamicSubagents = true
	library, err = runtime.ListAgentProfiles(t.Context())
	require.NoError(t, err)
	profile, found = agentLibraryEntry(library, "go-checker")
	require.True(t, found)
	assert.True(t, profile.Available)
	assert.Empty(t, profile.Unavailable)
}

func TestRuntimeRunAgentUsesUserVisibilityWithoutParentModelDelegation(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "direct-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Direct checker
description: Inspect one target when explicitly selected by the user.
visibility:
  user: true
  model: false
tools:
  allow: ["tool:read"]
output:
  format: text
---
PRIVATE DIRECT INSTRUCTIONS.
`),
		0o600,
	))

	model := newRuntimeModel(
		runtimeToolResponse("child-read", "read", `{"path":"target.txt"}`),
		runtimeTextResponse("direct verification complete"),
	)
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)
	require.NoError(t, os.WriteFile(
		filepath.Join(runtime.workspace.Root(), "target.txt"), []byte("verified\n"), 0o600,
	))

	result, err := runtime.RunAgent(t.Context(), AgentRunRequest{
		AgentID: "direct-checker", Task: "Inspect target.txt.",
	})
	require.NoError(t, err)
	assert.Equal(t, "direct-checker", result.Identity.ID)
	assert.Equal(t, subagent.OutcomeSucceeded, result.Outcome)
	assert.NotEmpty(t, result.ChildSessionID)
	assert.Equal(t, "direct verification complete", result.Value)

	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.Equal(t, []string{"read"}, toolNamesFromRequest(requests[0]))
	assert.NotContains(t, toolNamesFromRequest(requests[0]), subagent.ToolName)

	registry := runtime.integration.agentProfilesSnapshot()
	for _, definition := range registry.VisibleFor(agentprofile.AudienceModel) {
		assert.NotEqual(t, "direct-checker", definition.ID)
	}
	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, result.ChildSessionID, summaries[0].ChildSessionID)
}

func TestRuntimeRunAgentEventsStreamsSyntheticParentLifecycle(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "stream-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Stream checker
description: Return one bounded streaming result when explicitly selected.
visibility:
  user: true
  model: false
output:
  format: text
---
Return the bounded result.
`),
		0o600,
	))

	model := newRuntimeModel(runtimeTextResponse("streamed result"))
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)
	events := collectRuntimeEvents(t, runtime.RunAgentEvents(t.Context(), AgentRunRequest{
		AgentID: "stream-checker", Task: "Return the result.",
	}))

	types := eventTypes(events)
	assert.Contains(t, types, EventInteractionStarted)
	assert.Contains(t, types, EventRunStarted)
	assert.Contains(t, types, EventToolStarted)
	assert.Contains(t, types, EventSubagentCreated)
	assert.Contains(t, types, EventSubagentCompleted)
	assert.Contains(t, types, EventToolCompleted)
	assert.Contains(t, types, EventInteractionCompleted)
	assert.NotContains(t, types, EventMessageCommitted)
	for _, event := range events {
		if event.Type != EventToolStarted {
			continue
		}
		started, ok := event.Payload.(ToolStarted)
		require.True(t, ok)
		assert.Equal(t, subagent.ToolName, started.Call.Name)
	}

	requests := model.Requests()
	require.Len(t, requests, 1)
	assert.Empty(t, toolNamesFromRequest(requests[0]))
}

func TestRuntimeRunOneShotAgentNeverPublishesDefinition(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := newRuntimeModel(runtimeTextResponse("one-shot complete"))
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)
	definition := []byte(`---
schema: pips.agent/v1alpha1
name: Ad hoc checker
description: Perform one explicit ad hoc check.
visibility:
  user: false
  model: false
tools:
  allow: ["tool:read"]
output:
  format: text
---
This definition is intentionally non-persistent.
`)

	result, err := runtime.RunOneShotAgent(t.Context(), OneShotAgentRunRequest{
		AgentID: "adhoc-checker", Definition: definition, Task: "Report the bounded check.",
	})
	require.NoError(t, err)
	assert.Equal(t, subagent.AgentKindEphemeral, result.Identity.Kind)
	assert.Equal(t, "adhoc-checker", result.Identity.ID)
	assert.Equal(t, "one-shot:adhoc-checker", result.Identity.DefinitionSource)
	assert.Equal(t, "one-shot complete", result.Value)

	_, found := runtime.integration.agentProfilesSnapshot().Lookup("adhoc-checker")
	assert.False(t, found)
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(layout.AgentsDir(), "adhoc-checker.md"))
	require.ErrorIs(t, err, os.ErrNotExist)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, subagent.AgentKindEphemeral, summaries[0].Identity.Kind)
}

func TestRuntimeRunAgentFailsClosedWhenDirectChildNeedsQuestion(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "decision-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Decision checker
description: Ask for one decision before returning a bounded result.
visibility:
  user: true
  model: false
tools:
  allow: ["tool:ask_user"]
output:
  format: text
---
Ask for the missing decision before responding.
`),
		0o600,
	))

	model := newRuntimeModel(runtimeQuestionResponse(t, "child-question"))
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)

	result, err := runtime.RunAgent(t.Context(), AgentRunRequest{
		AgentID: "decision-checker", Task: "Ask for the framework.",
	})
	require.ErrorIs(t, err, ErrInputRequired)
	assert.Equal(t, subagent.OutcomeFailed, result.Outcome)
	assert.NotEmpty(t, result.ChildSessionID)

	summaries, listErr := runtime.ListSubagents(t.Context())
	require.NoError(t, listErr)
	require.Len(t, summaries, 1)
	assert.Equal(t, subagent.StateFailed, summaries[0].State)
	assert.Equal(t, result.ChildSessionID, summaries[0].ChildSessionID)
	assert.Len(t, model.Requests(), 1)
}

func TestRuntimeRunAgentFailsClosedWhenDirectChildNeedsApproval(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "shell-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Shell checker
description: Run one approved external verification.
visibility:
  user: true
  model: false
tools:
  allow: ["tool:shell"]
output:
  format: text
---
Run the requested verification only after approval.
`),
		0o600,
	))
	external := filepath.Join(base, "external")
	require.NoError(t, os.MkdirAll(external, 0o700))
	artifact := filepath.Join(external, "result.txt")
	shellArgs, err := json.Marshal(map[string]any{
		"command":       "printf child > " + artifact,
		"permissions":   map[string]any{"write_paths": []string{external}},
		"justification": "write the external test artifact",
	})
	require.NoError(t, err)

	model := newRuntimeModel(runtimeToolResponse("child-shell", "shell", string(shellArgs)))
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)

	result, runErr := runtime.RunAgent(t.Context(), AgentRunRequest{
		AgentID: "shell-checker", Task: "Run the approved check.",
	})
	require.ErrorIs(t, runErr, approval.ErrApprovalRequired)
	assert.Equal(t, subagent.OutcomeFailed, result.Outcome)
	assert.NotEmpty(t, result.ChildSessionID)
	_, statErr := os.Stat(artifact)
	require.ErrorIs(t, statErr, os.ErrNotExist)

	summaries, listErr := runtime.ListSubagents(t.Context())
	require.NoError(t, listErr)
	require.Len(t, summaries, 1)
	assert.Equal(t, subagent.StateFailed, summaries[0].State)
	assert.Equal(t, result.ChildSessionID, summaries[0].ChildSessionID)
	assert.Len(t, model.Requests(), 1)
}

func TestCustomChildChangeAuditStaysInChildSession(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "writer.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Writer
description: Make one approved, bounded workspace change.
tools:
  allow: ["tool:apply_patch"]
output:
  format: text
---
Make only the requested workspace edit and report completion.
`),
		0o600,
	))

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName, `{"agent_id":"writer","task":"Update target.txt."}`),
		runtimeToolResponse("child-patch", "apply_patch", `{"patch":"*** Begin Patch\n*** Update File: target.txt\n@@\n-before\n+after\n*** End Patch\n"}`),
		runtimeTextResponse("updated target"),
		runtimeTextResponse("parent complete"),
	)
	runtime := openFullAccessTestRuntimeConfiguredWithTrust(
		t,
		base,
		SessionTarget{},
		model,
		nil,
		nil,
		nil,
		true,
	)
	runtime.config.DynamicSubagents = true
	workspacePath := runtime.workspace.Root()
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "target.txt"), []byte("before\n"), 0o600))
	runGitTestCommand(t, runtime, "init")
	runGitTestCommand(t, runtime, "config", "user.name", "Pips Test")
	runGitTestCommand(t, runtime, "config", "user.email", "pips@example.invalid")
	runGitTestCommand(t, runtime, "add", "target.txt")
	runGitTestCommand(t, runtime, "commit", "-m", "initial")

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate the edit")))
	assert.NotContains(t, eventTypes(events), EventWorkspaceChanged)
	updated, err := os.ReadFile(filepath.Join(workspacePath, "target.txt"))
	require.NoError(t, err)
	assert.Equal(t, "after\n", string(updated))

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	childID := summaries[0].ChildSessionID
	childState, err := runtime.InspectSubagentState(t.Context(), childID)
	require.NoError(t, err)
	require.NotNil(t, childState.Changes)
	assert.Equal(t, "target.txt", childState.Changes.Entries[0].Path)

	child, err := runtime.repository.Open(t.Context(), session.OpenOptions{
		ID: childID, WorkspaceID: runtime.workspace.Identity().Key(),
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, child.Close()) }()
	var audit childWorkspaceChangeAudit
	found := false
	for _, entry := range child.Session().Entries() {
		if entry.Kind != harness.KindCustom || entry.Custom != childWorkspaceChangeAuditType {
			continue
		}
		require.NoError(t, json.Unmarshal(entry.Data, &audit))
		found = true
	}
	require.True(t, found)
	assert.Equal(t, "target.txt", audit.Report.Entries[0].Path)

	for _, entry := range runtime.handle.Session().Entries() {
		assert.NotEqual(t, childWorkspaceChangeAuditType, entry.Custom)
	}
}

func TestBackgroundCustomChildRetainsGenerationUntilTerminal(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "slow-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Slow checker
description: Hold one bounded background verification until completion.
delivery: [background]
tools:
  allow: ["tool:read"]
output:
  format: text
---
Inspect only the assigned target and return when the work is complete.
`),
		0o600,
	))

	lifecycle := &integrationTestLifecycle{}
	model := newBackgroundCustomRuntimeModel()
	runtime := openTestRuntimeAtWithExtensions(
		t,
		base,
		SessionTarget{},
		model,
		[]extension.Extension{integrationTestExtension(t, "custom-background", lifecycle)},
	)
	runtime.config.DynamicSubagents = true

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("start the background check")))
	select {
	case <-model.childStarted:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "custom background child did not start")
	}

	before := runtime.integration
	require.NoError(t, runtime.Reload(t.Context()))
	runtime.mu.Lock()
	after := runtime.integration
	runtime.mu.Unlock()
	assert.NotSame(t, before, after)
	assert.Zero(t, lifecycle.stops.Load(), "old generation must remain leased by the child")

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	close(model.releaseChild)
	_, err = runtime.WaitSubagent(t.Context(), summaries[0].ChildSessionID)
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return lifecycle.stops.Load() >= 1 }, time.Second, 10*time.Millisecond)
}

func agentLibraryEntry(library AgentLibrary, id string) (AgentLibraryEntry, bool) {
	for _, entry := range library.Entries {
		if entry.ID == id {
			return entry, true
		}
	}

	return AgentLibraryEntry{}, false
}

func TestRuntimeRoutesCustomChildApprovalWithoutPausingParentController(t *testing.T) {
	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "shell-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Shell checker
description: Run one approved shell verification.
tools:
  allow: ["tool:shell"]
output:
  format: text
---
Run the delegated verification and report the result.
`),
		0o600,
	))
	external := filepath.Join(base, "external")
	require.NoError(t, os.MkdirAll(external, 0o700))
	shellArgs, err := json.Marshal(map[string]any{
		"command":       "printf child > " + filepath.Join(external, "result.txt"),
		"permissions":   map[string]any{"write_paths": []string{external}},
		"justification": "write the verified external test artifact",
	})
	require.NoError(t, err)

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName, `{"agent_id":"shell-checker","task":"Run the approved check."}`),
		runtimeToolResponse("child-shell", "shell", string(shellArgs)),
		runtimeTextResponse("external verification complete"),
		runtimeTextResponse("parent complete"),
	)
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)

	done := make(chan error, 1)
	go func() {
		var sequenceErr error
		runtime.Prompt(context.Background(), ai.UserText("delegate the shell check"))(func(_ Event, err error) bool {
			if err != nil {
				sequenceErr = err

				return false
			}

			return true
		})
		done <- sequenceErr
	}()

	deadline := time.After(10 * time.Second)
	var (
		childID string
		state   ChildControlState
	)
	for childID == "" {
		summaries, listErr := runtime.ListSubagents(context.Background())
		if listErr == nil && len(summaries) == 1 {
			candidate := summaries[0].ChildSessionID
			control, controlErr := runtime.SubagentControlState(candidate)
			if controlErr == nil && control.Pause == ChildPauseApproval && control.Approval.Review != nil {
				childID, state = candidate, control
				break
			}
		}
		select {
		case <-deadline:
			require.FailNow(t, "timed out waiting for child approval")
		case <-time.After(10 * time.Millisecond):
		}
	}

	assert.Equal(t, approval.StateReview, state.Approval.Kind)
	assert.Equal(t, ApprovalNone, runtime.Snapshot().Approval.Kind)
	childState, err := runtime.InspectSubagentState(context.Background(), childID)
	require.NoError(t, err)
	assert.Equal(t, PhasePaused, childState.Phase)
	resolved, err := runtime.ResolveSubagentApproval(context.Background(), childID, approval.Resolution{
		RequestID: state.Approval.Review.RequestID,
		Choice:    approval.ChoiceAllowOnce,
	})
	require.NoError(t, err)
	assert.Equal(t, ChildPauseNone, resolved.Pause)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "timed out waiting for custom child completion")
	}
	contents, err := os.ReadFile(filepath.Join(external, "result.txt"))
	require.NoError(t, err)
	assert.Equal(t, "child", string(contents))

	parentState, err := runtime.controller.Reconcile(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, approval.StateReady, parentState.Kind)
}

func TestRuntimeRoutesCustomChildQuestionWithoutPausingParentController(t *testing.T) {
	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "decision-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Decision checker
description: Ask for one decision before producing a bounded summary.
tools:
  allow: ["tool:ask_user"]
output:
  format: text
---
Ask for the missing decision, then summarize the selected answer.
`),
		0o600,
	))

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName, `{"agent_id":"decision-checker","task":"Ask for the framework."}`),
		runtimeQuestionResponse(t, "child-question"),
		runtimeTextResponse("React selected"),
		runtimeTextResponse("parent complete"),
	)
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)

	done := make(chan error, 1)
	go func() {
		var sequenceErr error
		runtime.Prompt(context.Background(), ai.UserText("delegate the decision"))(func(_ Event, err error) bool {
			if err != nil {
				sequenceErr = err

				return false
			}

			return true
		})
		done <- sequenceErr
	}()

	deadline := time.After(10 * time.Second)
	var (
		childID string
		state   ChildControlState
	)
	for childID == "" {
		summaries, listErr := runtime.ListSubagents(context.Background())
		if listErr == nil && len(summaries) == 1 {
			candidate := summaries[0].ChildSessionID
			control, controlErr := runtime.SubagentControlState(candidate)
			if controlErr == nil && control.Pause == ChildPauseQuestion && control.Question != nil {
				childID, state = candidate, control
				break
			}
		}
		select {
		case <-deadline:
			require.FailNow(t, "timed out waiting for child question")
		case <-time.After(10 * time.Millisecond):
		}
	}

	request := question.CloneRequest(*state.Question)
	assert.Nil(t, runtime.Snapshot().Question.Required)
	childState, err := runtime.InspectSubagentState(context.Background(), childID)
	require.NoError(t, err)
	assert.Equal(t, PhasePaused, childState.Phase)
	resolved, err := runtime.ResolveSubagentQuestion(context.Background(), childID, question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	})
	require.NoError(t, err)
	assert.Equal(t, ChildPauseNone, resolved.Pause)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "timed out waiting for custom child completion")
	}

	summaries, err := runtime.ListSubagents(context.Background())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, subagent.StateSucceeded, summaries[0].State)
	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.Equal(t, []string{"ask_user"}, toolNamesFromRequest(requests[1]))
	assert.True(t, requestContainsToolText(requests[2], "React"))
}

func TestRuntimeRoutesNestedQuestionByExactDescendantWithoutParentContamination(t *testing.T) {
	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	for name, body := range map[string]string{
		"root-agent.md": `---
schema: pips.agent/v1alpha1
name: Root agent
description: Delegate one decision.
delegation:
  allow: [decision-agent]
---
Delegate the decision and return its result.
`,
		"decision-agent.md": `---
schema: pips.agent/v1alpha1
name: Decision agent
description: Ask one independently routed question.
visibility:
  user: false
  model: true
tools:
  allow: ["tool:ask_user"]
---
Ask for the framework and return the answer.
`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(layout.AgentsDir(), name), []byte(body), 0o600))
	}
	model := newRuntimeModel(
		runtimeToolResponse("parent-root", subagent.ToolName, `{"agent_id":"root-agent","task":"Get the decision."}`),
		runtimeToolResponse("root-decision", subagent.ToolName, `{"agent_id":"decision-agent","task":"Ask for the framework."}`),
		runtimeQuestionResponse(t, "nested-question"),
		runtimeTextResponse("React selected"),
		runtimeTextResponse("nested decision complete"),
		runtimeTextResponse("parent complete"),
	)
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.DynamicSubagents = true
			cfg.Subagent.MaxDepth = 1
		},
	)
	done := make(chan error, 1)
	go func() {
		var sequenceErr error
		runtime.Prompt(context.Background(), ai.UserText("run nested decision"))(func(_ Event, eventErr error) bool {
			if eventErr != nil {
				sequenceErr = eventErr

				return false
			}

			return true
		})
		done <- sequenceErr
	}()

	deadline := time.After(10 * time.Second)
	var rootID, decisionID string
	var state ChildControlState
	for decisionID == "" {
		summaries, listErr := runtime.ListSubagents(context.Background())
		if listErr == nil {
			for _, summary := range summaries {
				switch summary.Identity.ID {
				case "root-agent":
					rootID = summary.ChildSessionID
				case "decision-agent":
					control, controlErr := runtime.SubagentControlState(summary.ChildSessionID)
					if controlErr == nil && control.Pause == ChildPauseQuestion && control.Question != nil {
						decisionID, state = summary.ChildSessionID, control
					}
				}
			}
		}
		select {
		case <-deadline:
			require.FailNow(t, "timed out waiting for nested question")
		case <-time.After(10 * time.Millisecond):
		}
	}
	require.NotEmpty(t, rootID)
	request := question.CloneRequest(*state.Question)
	_, err = runtime.ResolveSubagentQuestion(context.Background(), rootID, question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	})
	require.Error(t, err)
	unchanged, err := runtime.SubagentControlState(decisionID)
	require.NoError(t, err)
	assert.Equal(t, request.ID, unchanged.Question.ID)
	assert.Nil(t, runtime.Snapshot().Question.Required)
	rootControl, err := runtime.SubagentControlState(rootID)
	require.NoError(t, err)
	assert.Equal(t, ChildPauseNone, rootControl.Pause)

	resolved, err := runtime.ResolveSubagentQuestion(context.Background(), decisionID, question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	})
	require.NoError(t, err)
	assert.Equal(t, ChildPauseNone, resolved.Pause)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "timed out waiting for nested question completion")
	}

	summaries, err := runtime.ListSubagents(context.Background())
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	for _, summary := range summaries {
		assert.Equal(t, subagent.StateSucceeded, summary.State)
	}
}

func TestRuntimeCancelsLiveNestedDescendantWithoutCancelingItsParent(t *testing.T) {
	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	for name, body := range map[string]string{
		"root-agent.md": `---
schema: pips.agent/v1alpha1
name: Root agent
description: Delegate one cancelable leaf.
delegation:
  allow: [leaf-agent]
---
ROOT_CANCEL_MARKER: delegate to leaf-agent and handle cancellation.
`,
		"leaf-agent.md": `---
schema: pips.agent/v1alpha1
name: Leaf agent
description: Wait until explicitly canceled.
visibility:
  user: false
  model: true
---
LEAF_CANCEL_MARKER: wait for cancellation.
`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(layout.AgentsDir(), name), []byte(body), 0o600))
	}
	model := newNestedCancelRuntimeModel()
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.DynamicSubagents = true
			cfg.Subagent.MaxDepth = 1
		},
	)
	done := make(chan error, 1)
	go func() {
		var sequenceErr error
		runtime.Prompt(context.Background(), ai.UserText("start nested cancellation"))(func(_ Event, eventErr error) bool {
			if eventErr != nil {
				sequenceErr = eventErr

				return false
			}

			return true
		})
		done <- sequenceErr
	}()
	select {
	case <-model.leafStarted:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "nested leaf did not start")
	}

	var leafID string
	assert.Eventually(t, func() bool {
		summaries, listErr := runtime.ListSubagents(context.Background())
		if listErr != nil {
			return false
		}
		for _, summary := range summaries {
			if summary.Identity.ID == "leaf-agent" {
				leafID = summary.ChildSessionID

				return true
			}
		}

		return false
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, runtime.CancelSubagent(context.Background(), leafID))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "nested cancellation did not complete")
	}

	summaries, err := runtime.ListSubagents(context.Background())
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	for _, summary := range summaries {
		switch summary.Identity.ID {
		case "leaf-agent":
			assert.Equal(t, subagent.StateCanceled, summary.State)
		case "root-agent":
			assert.Equal(t, subagent.StateSucceeded, summary.State)
		}
	}
}

func TestRuntimeRejectsCrossChildAndCrossKindControlSubstitution(t *testing.T) {
	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "approval-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Approval checker
description: Run one independently approved verification.
delivery: [background]
tools:
  allow: ["tool:shell"]
output:
  format: text
---
Run the requested verification exactly once and report completion.
`),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "question-checker.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Question checker
description: Ask one independently routed question.
delivery: [background]
tools:
  allow: ["tool:ask_user"]
output:
  format: text
---
Ask for the requested choice exactly once and report completion.
`),
		0o600,
	))
	external := filepath.Join(base, "external")
	require.NoError(t, os.MkdirAll(external, 0o700))

	model := newAuthorityRedTeamModel(t, external)
	t.Cleanup(model.release)
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("start the four isolated background checks")))
	assert.Equal(t, 4, countEventType(events, EventToolCompleted))

	select {
	case <-model.allChildrenStarted:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "timed out waiting for all red-team children to start")
	}
	model.release()

	expectedPause := map[string]ChildPauseKind{
		"approval-alpha": ChildPauseApproval,
		"approval-beta":  ChildPauseApproval,
		"question-alpha": ChildPauseQuestion,
		"question-beta":  ChildPauseQuestion,
	}
	childIDs, before := waitForRedTeamChildControls(t, runtime, expectedPause)

	approvalAlpha := before["approval-alpha"].Approval.Review
	approvalBeta := before["approval-beta"].Approval.Review
	questionAlpha := before["question-alpha"].Question
	questionBeta := before["question-beta"].Question
	require.NotNil(t, approvalAlpha)
	require.NotNil(t, approvalBeta)
	require.NotNil(t, questionAlpha)
	require.NotNil(t, questionBeta)

	_, err = runtime.ResolveSubagentApproval(t.Context(), childIDs["approval-beta"], approval.Resolution{
		RequestID: approvalAlpha.RequestID, Choice: approval.ChoiceAllowOnce,
	})
	require.Error(t, err)
	_, err = runtime.ResolveSubagentQuestion(t.Context(), childIDs["question-beta"], question.Resolution{
		RequestID: questionAlpha.ID, SchemaDigest: questionAlpha.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	})
	require.Error(t, err)
	_, err = runtime.RejectSubagentQuestion(
		t.Context(), childIDs["question-beta"], questionAlpha.ID, questionAlpha.SchemaDigest,
	)
	require.Error(t, err)
	_, err = runtime.ResolveSubagentApproval(t.Context(), childIDs["question-alpha"], approval.Resolution{
		RequestID: approvalAlpha.RequestID, Choice: approval.ChoiceAllowOnce,
	})
	require.Error(t, err)
	_, err = runtime.ResolveSubagentQuestion(t.Context(), childIDs["approval-alpha"], question.Resolution{
		RequestID: questionAlpha.ID, SchemaDigest: questionAlpha.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	})
	require.Error(t, err)
	_, err = runtime.RejectSubagentQuestion(
		t.Context(), childIDs["approval-beta"], questionAlpha.ID, questionAlpha.SchemaDigest,
	)
	require.Error(t, err)

	for task, expected := range before {
		actual, stateErr := runtime.SubagentControlState(childIDs[task])
		require.NoError(t, stateErr)
		assert.Equal(t, expected, actual, "malicious resolution changed %s", task)
	}
	assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)
	assert.Equal(t, ApprovalNone, runtime.Snapshot().Approval.Kind)
	assert.Nil(t, runtime.Snapshot().Question.Required)
	for _, task := range []string{"approval-alpha", "approval-beta"} {
		_, statErr := os.Stat(filepath.Join(external, task+".txt"))
		require.ErrorIs(t, statErr, os.ErrNotExist)
	}
	for task := range expectedPause {
		assert.Len(t, model.requestsFor(task), 1, "malicious resolution resumed %s", task)
	}

	for task, review := range map[string]*approval.Review{
		"approval-alpha": approvalAlpha,
		"approval-beta":  approvalBeta,
	} {
		_, err = runtime.ResolveSubagentApproval(t.Context(), childIDs[task], approval.Resolution{
			RequestID: review.RequestID, Choice: approval.ChoiceAllowOnce,
		})
		require.NoError(t, err)
		_, err = runtime.ResolveSubagentApproval(t.Context(), childIDs[task], approval.Resolution{
			RequestID: review.RequestID, Choice: approval.ChoiceAllowOnce,
		})
		require.Error(t, err, "duplicate approval resolution must fail")
	}
	for task, request := range map[string]*question.Request{
		"question-alpha": questionAlpha,
		"question-beta":  questionBeta,
	} {
		resolution := question.Resolution{
			RequestID: request.ID, SchemaDigest: request.SchemaDigest,
			Answers: []question.Answer{{Selections: []string{"React"}}},
		}
		_, err = runtime.ResolveSubagentQuestion(t.Context(), childIDs[task], resolution)
		require.NoError(t, err)
		_, err = runtime.ResolveSubagentQuestion(t.Context(), childIDs[task], resolution)
		require.Error(t, err, "duplicate question resolution must fail")
	}

	waitForRedTeamChildrenTerminal(t, runtime, childIDs)
	for task, review := range map[string]*approval.Review{
		"approval-alpha": approvalAlpha,
		"approval-beta":  approvalBeta,
	} {
		_, err = runtime.ResolveSubagentApproval(t.Context(), childIDs[task], approval.Resolution{
			RequestID: review.RequestID, Choice: approval.ChoiceAllowOnce,
		})
		require.ErrorIs(t, err, ErrRuntimeNotPaused)
		contents, readErr := os.ReadFile(filepath.Join(external, task+".txt"))
		require.NoError(t, readErr)
		assert.Equal(t, task, string(contents), "approved command executed more than once")
	}
	for task, request := range map[string]*question.Request{
		"question-alpha": questionAlpha,
		"question-beta":  questionBeta,
	} {
		_, err = runtime.ResolveSubagentQuestion(t.Context(), childIDs[task], question.Resolution{
			RequestID: request.ID, SchemaDigest: request.SchemaDigest,
			Answers: []question.Answer{{Selections: []string{"React"}}},
		})
		require.ErrorIs(t, err, ErrRuntimeNotPaused)
	}
	for task := range expectedPause {
		assert.Len(t, model.requestsFor(task), 2, "%s must receive one tool turn and one final turn", task)
	}
}

func waitForRedTeamChildControls(
	t *testing.T,
	runtime *Runtime,
	expected map[string]ChildPauseKind,
) (map[string]string, map[string]ChildControlState) {
	t.Helper()

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		childIDs := make(map[string]string, len(expected))
		states := make(map[string]ChildControlState, len(expected))
		summaries, err := runtime.ListSubagents(t.Context())
		if err == nil {
			for _, summary := range summaries {
				pause, exists := expected[summary.TaskPreview]
				if !exists {
					continue
				}
				state, stateErr := runtime.SubagentControlState(summary.ChildSessionID)
				if stateErr == nil && state.Pause == pause {
					childIDs[summary.TaskPreview] = summary.ChildSessionID
					states[summary.TaskPreview] = state
				}
			}
		}
		if len(states) == len(expected) {
			return childIDs, states
		}

		select {
		case <-deadline.C:
			require.FailNow(t, "timed out waiting for live red-team child controls")
		case <-ticker.C:
		case <-t.Context().Done():
			require.FailNow(t, "test context canceled while waiting for child controls")
		}
	}
}

func waitForRedTeamChildrenTerminal(t *testing.T, runtime *Runtime, childIDs map[string]string) {
	t.Helper()

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		terminal := make(map[string]subagent.State, len(childIDs))
		summaries, err := runtime.ListSubagents(t.Context())
		if err == nil {
			for _, summary := range summaries {
				for task, childID := range childIDs {
					if summary.ChildSessionID == childID && redTeamTerminalState(summary.State) {
						terminal[task] = summary.State
					}
				}
			}
		}
		if len(terminal) == len(childIDs) {
			for task, state := range terminal {
				assert.Equal(t, subagent.StateSucceeded, state, task)
			}

			return
		}

		select {
		case <-deadline.C:
			require.FailNow(t, "timed out waiting for red-team children to finish")
		case <-ticker.C:
		case <-t.Context().Done():
			require.FailNow(t, "test context canceled while waiting for terminal children")
		}
	}
}

func redTeamTerminalState(state subagent.State) bool {
	switch state {
	case subagent.StateSucceeded, subagent.StateFailed, subagent.StateCanceled, subagent.StateInterrupted:
		return true
	default:
		return false
	}
}

func TestRuntimeRecoveryRejectsStaleCustomChildControls(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		args     ai.JSON
		resolve  func(context.Context, *Runtime, string) error
	}{
		{
			name:     "approval",
			toolName: "shell",
			args:     ai.JSON(`{"command":"printf must-not-run","permissions":{"network":true}}`),
			resolve: func(ctx context.Context, runtime *Runtime, childID string) error {
				_, err := runtime.ResolveSubagentApproval(ctx, childID, approval.Resolution{
					RequestID: "stale-child-call", Choice: approval.ChoiceAllowOnce,
				})

				return err
			},
		},
		{
			name:     "question",
			toolName: "ask_user",
			args: ai.JSON(
				`{"questions":[{"header":"UI","prompt":"Choose","options":[{"label":"A","description":"first"},{"label":"B","description":"second"}]}]}`,
			),
			resolve: func(ctx context.Context, runtime *Runtime, childID string) error {
				_, err := runtime.ResolveSubagentQuestion(ctx, childID, question.Resolution{
					RequestID: "stale-child-call", SchemaDigest: strings.Repeat("a", 64),
					Answers: []question.Answer{{Selections: []string{"A"}}},
				})

				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			first := openDynamicTestRuntimeAt(
				t, base, SessionTarget{RetainEmpty: true}, newRuntimeModel(),
			)
			parentMeta := first.handle.Metadata()
			require.NoError(t, first.Close(t.Context()))

			repository, err := session.NewRepository(first.paths.SessionsDir())
			require.NoError(t, err)
			parent, err := repository.Open(t.Context(), session.OpenOptions{
				ID: parentMeta.ID, WorkspaceID: parentMeta.WorkspaceID,
			})
			require.NoError(t, err)
			child, err := repository.Create(t.Context(), session.CreateOptions{
				WorkspaceID: parentMeta.WorkspaceID, WorkspacePath: parentMeta.WorkspacePath,
				Kind: session.KindSubagent, ParentSessionID: parentMeta.ID,
				ParentRunID: "parent-run", Agent: string(subagent.RoleExplore),
			})
			require.NoError(t, err)
			childID := child.Metadata().ID
			appendInterruptedChildLifecycle(t, parent.Session(), child.Session(), childID, parentMeta.ID)
			_, err = child.Session().AppendMessage(ai.Assistant(ai.ToolCallPart{
				ID: "stale-child-call", Name: test.toolName, Args: test.args,
			}), nil)
			require.NoError(t, err)
			require.NoError(t, child.Close())
			require.NoError(t, parent.Close())

			reopenedModel := newRuntimeModel()
			reopened := openDynamicTestRuntimeAt(
				t, base, SessionTarget{ID: parentMeta.ID}, reopenedModel,
			)
			detail, err := reopened.InspectSubagent(t.Context(), childID)
			require.NoError(t, err)
			assert.Equal(t, subagent.StateInterrupted, detail.Summary.State)
			_, err = reopened.SubagentControlState(childID)
			require.ErrorIs(t, err, ErrRuntimeNotPaused)
			require.ErrorIs(t, test.resolve(t.Context(), reopened, childID), ErrRuntimeNotPaused)

			persisted, err := repository.Open(t.Context(), session.OpenOptions{
				ID: childID, WorkspaceID: parentMeta.WorkspaceID,
			})
			require.NoError(t, err)
			pending, err := persisted.Session().Pending()
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, "stale-child-call", pending[0].ID)
			assert.Equal(t, test.toolName, pending[0].Name)
			require.NoError(t, persisted.Close())
			assert.Empty(t, reopenedModel.Requests())
		})
	}
}

func appendInterruptedChildLifecycle(
	t *testing.T,
	parent *harness.Session,
	child *harness.Session,
	childID string,
	parentID string,
) {
	t.Helper()

	limits := subagent.ProductionLimits()
	record := map[string]any{
		"schema":            "pips.coding.subagent.record/v1alpha1",
		"state":             string(subagent.StateCreated),
		"role":              string(subagent.RoleExplore),
		"child_session_id":  childID,
		"parent_session_id": parentID,
		"parent_run_id":     "parent-run",
		"model":             "openai/runtime-test",
		"limits": map[string]any{
			"max_turns": limits.MaxTurns, "finalization_turns": limits.FinalizationTurns,
			"repeated_tool_call_limit": limits.RepeatedToolCallLimit,
			"max_tokens":               limits.MaxTokens, "max_tool_calls": limits.MaxToolCalls,
			"max_duration_nanos": int64(limits.MaxDuration),
			"max_activity_tools": limits.MaxActivityTools,
			"max_output_tokens":  limits.MaxOutputTokens, "max_task_bytes": limits.MaxTaskBytes,
			"max_result_bytes": limits.MaxResultBytes, "max_result_items": limits.MaxResultItems,
			"max_field_bytes": limits.MaxFieldBytes,
		},
		"usage": map[string]any{},
		"time":  time.Now().UTC(),
	}
	appendRecord := func(target *harness.Session, customType string) {
		data, err := json.Marshal(record)
		require.NoError(t, err)
		_, err = target.AppendCustom(customType, ai.JSON(data))
		require.NoError(t, err)
	}
	appendRecord(child, "pips.coding.subagent.created")
	appendRecord(parent, "pips.coding.subagent.created")
	record["state"] = string(subagent.StateRunning)
	record["child_run_id"] = "child-run"
	record["time"] = time.Now().UTC().Add(time.Millisecond)
	appendRecord(child, "pips.coding.subagent.started")
	appendRecord(parent, "pips.coding.subagent.started")
}

func openDynamicTestRuntimeAt(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
) *Runtime {
	t.Helper()

	runtime := openTestRuntimeAt(t, base, target, model)
	runtime.config.DynamicSubagents = true

	return runtime
}

type backgroundCustomRuntimeModel struct {
	mu           sync.Mutex
	mainCalls    int
	childOnce    sync.Once
	childStarted chan struct{}
	releaseChild chan struct{}
}

type nestedCancelRuntimeModel struct {
	mu          sync.Mutex
	mainCalls   int
	rootCalls   int
	leafOnce    sync.Once
	leafStarted chan struct{}
}

func newNestedCancelRuntimeModel() *nestedCancelRuntimeModel {
	return &nestedCancelRuntimeModel{leafStarted: make(chan struct{})}
}

func (m *nestedCancelRuntimeModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	switch {
	case strings.Contains(request.System, "LEAF_CANCEL_MARKER"):
		m.leafOnce.Do(func() { close(m.leafStarted) })
		<-ctx.Done()

		return nil, ctx.Err()
	case strings.Contains(request.System, "ROOT_CANCEL_MARKER"):
		m.mu.Lock()
		m.rootCalls++
		call := m.rootCalls
		m.mu.Unlock()
		if call == 1 {
			return runtimeToolResponse(
				"root-leaf",
				subagent.ToolName,
				`{"agent_id":"leaf-agent","task":"Wait until canceled."}`,
			), nil
		}

		return runtimeTextResponse("root handled leaf cancellation"), nil
	default:
		m.mu.Lock()
		m.mainCalls++
		call := m.mainCalls
		m.mu.Unlock()
		if call == 1 {
			return runtimeToolResponse(
				"parent-root",
				subagent.ToolName,
				`{"agent_id":"root-agent","task":"Run the cancelable leaf."}`,
			), nil
		}

		return runtimeTextResponse("parent complete"), nil
	}
}

func (m *nestedCancelRuntimeModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)

			return
		}
		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (m *nestedCancelRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }

func (m *nestedCancelRuntimeModel) ModelID() string { return "runtime-test" }

func (m *nestedCancelRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func newBackgroundCustomRuntimeModel() *backgroundCustomRuntimeModel {
	return &backgroundCustomRuntimeModel{
		childStarted: make(chan struct{}),
		releaseChild: make(chan struct{}),
	}
}

func (m *backgroundCustomRuntimeModel) Generate(
	ctx context.Context,
	request ai.Request,
) (*ai.Response, error) {
	if strings.Contains(request.System, "You are a specialized child agent") {
		m.childOnce.Do(func() { close(m.childStarted) })
		select {
		case <-m.releaseChild:
			return runtimeTextResponse("background custom result"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	m.mu.Lock()
	m.mainCalls++
	call := m.mainCalls
	m.mu.Unlock()
	switch call {
	case 1:
		return runtimeToolResponse(
			"spawn-custom",
			subagent.SpawnToolName,
			`{"agent_id":"slow-checker","task":"Inspect in the background."}`,
		), nil
	case 2:
		return runtimeTextResponse("parent continued"), nil
	default:
		return runtimeTextResponse("background completion handled"), nil
	}
}

func (m *backgroundCustomRuntimeModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)

			return
		}
		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (*backgroundCustomRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*backgroundCustomRuntimeModel) ModelID() string       { return "runtime-test" }
func (*backgroundCustomRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = (*backgroundCustomRuntimeModel)(nil)

type authorityRedTeamTask struct {
	agentID   string
	pause     ChildPauseKind
	shellArgs string
}

type authorityRedTeamModel struct {
	mu sync.Mutex

	tasks              map[string]authorityRedTeamTask
	spawnOrder         []string
	parentCalls        int
	childCalls         map[string]int
	childRequests      map[string][]ai.Request
	startedChildren    int
	allChildrenStarted chan struct{}
	releaseChildren    chan struct{}
	releaseOnce        sync.Once
}

func newAuthorityRedTeamModel(t *testing.T, external string) *authorityRedTeamModel {
	t.Helper()

	tasks := map[string]authorityRedTeamTask{
		"approval-alpha": {agentID: "approval-checker", pause: ChildPauseApproval},
		"approval-beta":  {agentID: "approval-checker", pause: ChildPauseApproval},
		"question-alpha": {agentID: "question-checker", pause: ChildPauseQuestion},
		"question-beta":  {agentID: "question-checker", pause: ChildPauseQuestion},
	}
	for _, task := range []string{"approval-alpha", "approval-beta"} {
		args, err := json.Marshal(map[string]any{
			"command": "printf " + task + " >> " + filepath.Join(external, task+".txt"),
			"permissions": map[string]any{
				"write_paths": []string{external},
			},
			"justification": "write one red-team verification artifact",
		})
		require.NoError(t, err)
		value := tasks[task]
		value.shellArgs = string(args)
		tasks[task] = value
	}

	return &authorityRedTeamModel{
		tasks: tasks,
		spawnOrder: []string{
			"approval-alpha", "approval-beta", "question-alpha", "question-beta",
		},
		childCalls:         make(map[string]int, len(tasks)),
		childRequests:      make(map[string][]ai.Request, len(tasks)),
		allChildrenStarted: make(chan struct{}),
		releaseChildren:    make(chan struct{}),
	}
}

func (m *authorityRedTeamModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	if strings.Contains(request.System, "You are a specialized child agent") {
		return m.generateChild(ctx, request)
	}

	return m.generateParent()
}

func (m *authorityRedTeamModel) generateChild(ctx context.Context, request ai.Request) (*ai.Response, error) {
	for task, definition := range m.tasks {
		if !requestContainsText(request, task) {
			continue
		}

		m.mu.Lock()
		m.childCalls[task]++
		call := m.childCalls[task]
		m.childRequests[task] = append(m.childRequests[task], request)
		if call == 1 {
			m.startedChildren++
		}
		becameAllStarted := call == 1 && m.startedChildren == len(m.tasks)
		m.mu.Unlock()
		if becameAllStarted {
			close(m.allChildrenStarted)
		}

		if call > 1 {
			return runtimeTextResponse(task + " complete"), nil
		}
		select {
		case <-m.releaseChildren:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		switch definition.pause {
		case ChildPauseApproval:
			return runtimeToolResponse(task+"-approval", "shell", definition.shellArgs), nil
		case ChildPauseQuestion:
			args, err := json.Marshal(question.Spec{Questions: []question.Question{{
				Header: "Framework", Question: "Which framework should be used for " + task + "?",
				Options: []question.Option{
					{Label: "React", Description: "Established ecosystem"},
					{Label: "Vue", Description: "Progressive framework"},
				},
			}}})
			if err != nil {
				return nil, err
			}

			return runtimeToolResponse(task+"-question", question.ToolName, string(args)), nil
		default:
			return nil, fmt.Errorf("unexpected red-team pause kind %q", definition.pause)
		}
	}

	return nil, errors.New("red-team child request has no task marker")
}

func (m *authorityRedTeamModel) generateParent() (*ai.Response, error) {
	m.mu.Lock()
	m.parentCalls++
	call := m.parentCalls
	if call <= len(m.spawnOrder) {
		task := m.spawnOrder[call-1]
		definition := m.tasks[task]
		m.mu.Unlock()
		args, err := json.Marshal(map[string]string{"agent_id": definition.agentID, "task": task})
		if err != nil {
			return nil, err
		}

		return runtimeToolResponse("spawn-"+task, subagent.SpawnToolName, string(args)), nil
	}
	m.mu.Unlock()

	return runtimeTextResponse("red-team parent complete"), nil
}

func (m *authorityRedTeamModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)

			return
		}
		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (*authorityRedTeamModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*authorityRedTeamModel) ModelID() string       { return "runtime-test" }
func (*authorityRedTeamModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func (m *authorityRedTeamModel) requestsFor(task string) []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ai.Request(nil), m.childRequests[task]...)
}

func (m *authorityRedTeamModel) release() {
	m.releaseOnce.Do(func() { close(m.releaseChildren) })
}

var _ ai.LanguageModel = (*authorityRedTeamModel)(nil)
