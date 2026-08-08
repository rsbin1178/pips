//nolint:wsl_v5,paralleltest,gosec // Temporal child-control fixtures and bounded test paths stay explicit.
package coding

import (
	"context"
	"encoding/json"
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
