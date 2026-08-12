//nolint:wsl_v5,paralleltest // Draft lifecycle tests keep private persistence assertions sequential and explicit.
package coding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeGeneratesIsolatedAgentDraftAndPromotesOnlyAfterExplicitReview(t *testing.T) {
	const (
		agentID       = "draft-reviewer"
		intentMarker  = "PRIVATE_DRAFT_INTENT_MARKER"
		contentMarker = "PRIVATE_DRAFT_CONTENT_MARKER"
	)
	definition := testAgentDraftDefinition("Draft reviewer", contentMarker)
	model := newRuntimeModel(testAgentDraftProposalResponse(t, definition))
	base := t.TempDir()
	observed := make([]TelemetryEvent, 0, 8)
	runtime := openTestRuntimeConfigured(
		t,
		base,
		SessionTarget{},
		model,
		nil,
		nil,
		[]TelemetryObserver{TelemetryObserverFunc(func(_ context.Context, event TelemetryEvent) error {
			observed = append(observed, event)

			return nil
		})},
	)
	runtime.config.DynamicSubagents = true
	runtime.requestPolicy = func(request *ai.Request) {
		request.Tools = []ai.Tool{{Name: "policy_injected_tool"}}
		request.MaxTokens = ai.Ptr(1_000_000)
	}
	target := filepath.Join(runtime.paths.AgentsDir(), agentID+".md")

	draft, err := runtime.GenerateAgentDraft(t.Context(), GenerateAgentDraftRequest{
		AgentID: agentID, Intent: intentMarker, Scope: AgentDraftScopeUser,
	})
	require.NoError(t, err)
	assert.Equal(t, agentID, draft.Summary.AgentID)
	assert.Equal(t, definition, string(draft.Definition))
	assert.NotEmpty(t, draft.Summary.DefinitionDigest)
	assert.NotEmpty(t, draft.Preview.PlanDigest)
	assert.Equal(t, runtime.generationID, draft.Preview.GenerationID)
	_, err = os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist)
	children, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	assert.Empty(t, children)

	requests := model.Requests()
	require.Len(t, requests, 1)
	assert.Equal(t, []string{agentDraftToolName}, toolNamesFromRequest(requests[0]))
	require.NotNil(t, requests[0].MaxTokens)
	assert.Equal(t, agentDraftAgentMaxOutputTokens, *requests[0].MaxTokens)
	assert.Contains(t, requestSystemText(requests[0]), intentMarker)
	assert.NotContains(t, requestSystemText(requests[0]), contentMarker)

	entriesJSON, err := json.Marshal(runtime.handle.Session().Entries())
	require.NoError(t, err)
	assert.NotContains(t, string(entriesJSON), intentMarker)
	assert.NotContains(t, string(entriesJSON), contentMarker)
	telemetryJSON, err := json.Marshal(observed)
	require.NoError(t, err)
	assert.NotContains(t, string(telemetryJSON), intentMarker)
	assert.NotContains(t, string(telemetryJSON), contentMarker)
	library, err := runtime.ListAgentProfiles(t.Context())
	require.NoError(t, err)
	assert.False(t, agentLibraryContains(library, agentID))

	summaries, err := runtime.ListAgentDrafts(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, draft.Summary, summaries[0])
	assert.NotContains(t, mustJSON(t, summaries), contentMarker)
	inspected, err := runtime.InspectAgentDraft(t.Context(), draft.Summary.DraftID)
	require.NoError(t, err)
	inspected.Definition[0] = 'x'
	inspected.Preview.DeclaredTools = append(inspected.Preview.DeclaredTools, "allow:tool:injected")
	again, err := runtime.InspectAgentDraft(t.Context(), draft.Summary.DraftID)
	require.NoError(t, err)
	assert.Equal(t, definition, string(again.Definition))
	assert.NotContains(t, again.Preview.DeclaredTools, "allow:tool:injected")

	promotion, err := runtime.PromoteAgentDraft(t.Context(), PromoteAgentDraftRequest{
		DraftID: draft.Summary.DraftID, ExpectedDigest: draft.Summary.DefinitionDigest,
	})
	require.NoError(t, err)
	assert.Equal(t, target, promotion.Target)
	assert.True(t, promotion.ReloadRequired)
	// #nosec G304 -- target is the validated Runtime-owned test Agent path.
	contents, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, definition, string(contents))
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	library, err = runtime.ListAgentProfiles(t.Context())
	require.NoError(t, err)
	assert.False(t, agentLibraryContains(library, agentID), "promotion mutated the current generation")
	_, err = runtime.InspectAgentDraft(t.Context(), draft.Summary.DraftID)
	require.ErrorIs(t, err, ErrAgentDraft)

	require.NoError(t, runtime.Reload(t.Context()))
	library, err = runtime.ListAgentProfiles(t.Context())
	require.NoError(t, err)
	assert.True(t, agentLibraryContains(library, agentID))
}

func TestRuntimeAgentDraftPromotionRejectsStaleEditAndOverwriteWithoutConsumingDraft(t *testing.T) {
	const agentID = "editable-reviewer"
	original := testAgentDraftDefinition("Original", "original instructions")
	model := newRuntimeModel(testAgentDraftProposalResponse(t, original))
	base := t.TempDir()
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)
	draft, err := runtime.GenerateAgentDraft(t.Context(), GenerateAgentDraftRequest{
		AgentID: agentID, Intent: "Generate an editable reviewer.", Scope: AgentDraftScopeUser,
	})
	require.NoError(t, err)

	_, err = runtime.PromoteAgentDraft(t.Context(), PromoteAgentDraftRequest{
		DraftID: draft.Summary.DraftID, ExpectedDigest: strings.Repeat("0", 64),
	})
	require.ErrorIs(t, err, ErrAgentDraft)
	_, err = runtime.InspectAgentDraft(t.Context(), draft.Summary.DraftID)
	require.NoError(t, err)

	edited := testAgentDraftDefinition("Edited", "edited instructions")
	target := filepath.Join(runtime.paths.AgentsDir(), agentID+".md")
	require.NoError(t, os.MkdirAll(runtime.paths.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(target, []byte("existing"), 0o600))
	_, err = runtime.PromoteAgentDraft(t.Context(), PromoteAgentDraftRequest{
		DraftID: draft.Summary.DraftID, ExpectedDigest: draft.Summary.DefinitionDigest,
		Definition: []byte(edited),
	})
	require.ErrorIs(t, err, ErrAgentDraft)
	// #nosec G304 -- target is the validated Runtime-owned test Agent path.
	contents, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, "existing", string(contents))
	retained, err := runtime.InspectAgentDraft(t.Context(), draft.Summary.DraftID)
	require.NoError(t, err)
	assert.Equal(t, original, string(retained.Definition))
}

func TestRuntimePromotesEditedAgentDraftToTrustedProjectRoot(t *testing.T) {
	const agentID = "project-reviewer"
	original := testAgentDraftDefinition("Original project", "original project instructions")
	edited := testAgentDraftDefinition("Edited project", "edited project instructions")
	model := newRuntimeModel(testAgentDraftProposalResponse(t, original))
	base := t.TempDir()
	runtime := openTestRuntimeConfiguredWithTrust(
		t, base, SessionTarget{}, model, nil, nil, nil, true,
	)
	runtime.config.DynamicSubagents = true

	draft, err := runtime.GenerateAgentDraft(t.Context(), GenerateAgentDraftRequest{
		AgentID: agentID, Intent: "Generate a project reviewer.", Scope: AgentDraftScopeProject,
	})
	require.NoError(t, err)
	promotion, err := runtime.PromoteAgentDraft(t.Context(), PromoteAgentDraftRequest{
		DraftID: draft.Summary.DraftID, ExpectedDigest: draft.Summary.DefinitionDigest,
		Definition: []byte(edited),
	})
	require.NoError(t, err)
	assert.Equal(t, ".pips/agents/"+agentID+".md", promotion.Target)
	assert.NotEqual(t, draft.Summary.DefinitionDigest, promotion.DefinitionDigest)
	contents, err := os.ReadFile(filepath.Join(runtime.workspace.Root(), filepath.FromSlash(promotion.Target)))
	require.NoError(t, err)
	assert.Equal(t, edited, string(contents))
}

func TestRuntimeAgentDraftPromotionRejectsSymlinkUserDirectory(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink creation requires host privilege on Windows")
	}

	const agentID = "symlink-reviewer"
	definition := testAgentDraftDefinition("Symlink", "symlink instructions")
	model := newRuntimeModel(testAgentDraftProposalResponse(t, definition))
	runtime := openDynamicTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	draft, err := runtime.GenerateAgentDraft(t.Context(), GenerateAgentDraftRequest{
		AgentID: agentID, Intent: "Generate a symlink test reviewer.", Scope: AgentDraftScopeUser,
	})
	require.NoError(t, err)
	external := t.TempDir()
	require.NoError(t, os.Symlink(external, runtime.paths.AgentsDir()))

	_, err = runtime.PromoteAgentDraft(t.Context(), PromoteAgentDraftRequest{
		DraftID: draft.Summary.DraftID, ExpectedDigest: draft.Summary.DefinitionDigest,
	})
	require.ErrorIs(t, err, ErrAgentDraft)
	_, err = os.Stat(filepath.Join(external, agentID+".md"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = runtime.InspectAgentDraft(t.Context(), draft.Summary.DraftID)
	require.NoError(t, err)
}

func TestAgentDraftCollectorAcceptsExactlyOneStrictProposal(t *testing.T) {
	definition := testAgentDraftDefinition("Collector", "collector instructions")
	collector, err := newAgentDraftCollector("collector", agentDraftTestLimits())
	require.NoError(t, err)
	arguments := mustJSON(t, agentDraftProposalRequest{Definition: definition})

	_, err = collector.Exec(t.Context(), agent.ToolCall{Name: agentDraftToolName, Args: ai.JSON(arguments)})
	require.ErrorIs(t, err, agent.ErrTerminate)
	parsedBytes, parsed, err := collector.proposal()
	require.NoError(t, err)
	assert.Equal(t, definition, string(parsedBytes))
	assert.Equal(t, "collector", parsed.ID)

	_, err = collector.Exec(t.Context(), agent.ToolCall{Name: agentDraftToolName, Args: ai.JSON(arguments)})
	require.ErrorIs(t, err, agent.ErrTerminate)
	_, _, err = collector.proposal()
	require.ErrorIs(t, err, ErrAgentDraft)

	invalid, err := newAgentDraftCollector("invalid", agentDraftTestLimits())
	require.NoError(t, err)
	_, err = invalid.Exec(t.Context(), agent.ToolCall{
		Name: agentDraftToolName, Args: ai.JSON(`{"definition":"not markdown","extra":true}`),
	})
	require.ErrorIs(t, err, agent.ErrTerminate)
	_, _, err = invalid.proposal()
	require.ErrorIs(t, err, ErrAgentDraft)
}

func TestRuntimeRejectsProjectAgentDraftForUntrustedWorkspace(t *testing.T) {
	model := newRuntimeModel()
	runtime := openDynamicTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)

	_, err := runtime.GenerateAgentDraft(t.Context(), GenerateAgentDraftRequest{
		AgentID: "untrusted-project", Intent: "Generate project Agent.", Scope: AgentDraftScopeProject,
	})
	require.ErrorIs(t, err, ErrAgentDraft)
	assert.Empty(t, model.Requests())
}

func TestRuntimeRejectsAgentDraftThatCollidesWithCurrentRegistry(t *testing.T) {
	const agentID = "existing-review"
	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), agentID+".md"),
		[]byte(testAgentDraftDefinition("Existing", "existing instructions")),
		0o600,
	))
	model := newRuntimeModel(testAgentDraftProposalResponse(
		t,
		testAgentDraftDefinition("Replacement", "replacement instructions"),
	))
	runtime := openDynamicTestRuntimeAt(t, base, SessionTarget{}, model)

	_, err = runtime.GenerateAgentDraft(t.Context(), GenerateAgentDraftRequest{
		AgentID: agentID, Intent: "Replace the existing Agent.", Scope: AgentDraftScopeUser,
	})
	require.ErrorIs(t, err, ErrAgentDraft)
	drafts, err := runtime.ListAgentDrafts(t.Context())
	require.NoError(t, err)
	assert.Empty(t, drafts)
}

func TestRuntimeAgentDraftPreviewCannotExpandCurrentAuthority(t *testing.T) {
	allowedOnly := strings.Replace(
		testAgentDraftDefinition("Unavailable", "unavailable instructions"),
		`allow: ["tool:read"]`,
		`allow: ["tool:not-present"]`,
		1,
	)
	model := newRuntimeModel(testAgentDraftProposalResponse(t, allowedOnly))
	runtime := openDynamicTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	draft, err := runtime.GenerateAgentDraft(t.Context(), GenerateAgentDraftRequest{
		AgentID: "unavailable-allow", Intent: "Request an unavailable Tool.",
		Scope: AgentDraftScopeUser,
	})
	require.NoError(t, err)
	assert.Contains(t, draft.Preview.DeclaredTools, "allow:tool:not-present")
	assert.Empty(t, draft.Preview.Capabilities)

	required := strings.Replace(
		allowedOnly,
		`allow: ["tool:not-present"]`,
		"allow: [\"tool:not-present\"]\n  require: [\"tool:not-present\"]",
		1,
	)
	model = newRuntimeModel(testAgentDraftProposalResponse(t, required))
	runtime = openDynamicTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	_, err = runtime.GenerateAgentDraft(t.Context(), GenerateAgentDraftRequest{
		AgentID: "unavailable-require", Intent: "Require an unavailable Tool.",
		Scope: AgentDraftScopeUser,
	})
	require.ErrorIs(t, err, ErrAgentDraft)
	drafts, err := runtime.ListAgentDrafts(t.Context())
	require.NoError(t, err)
	assert.Empty(t, drafts)
}

func TestRuntimeCloseDropsProcessLocalAgentDrafts(t *testing.T) {
	definition := testAgentDraftDefinition("Close", "close instructions")
	model := newRuntimeModel(testAgentDraftProposalResponse(t, definition))
	runtime := openDynamicTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	_, err := runtime.GenerateAgentDraft(t.Context(), GenerateAgentDraftRequest{
		AgentID: "close-review", Intent: "Generate then close.", Scope: AgentDraftScopeUser,
	})
	require.NoError(t, err)
	require.NotEmpty(t, runtime.agentDrafts)

	require.NoError(t, runtime.Close(t.Context()))
	assert.Nil(t, runtime.agentDrafts)
}

func FuzzDecodeAgentDraftProposal(f *testing.F) {
	limits := agentprofile.DefaultLimits()
	valid, err := json.Marshal(agentDraftProposalRequest{
		Definition: testAgentDraftDefinition("Fuzz", "fuzz instructions"),
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte(`{"definition":"not markdown"}`))
	f.Add([]byte(`{"definition":"x","unknown":true}`))
	f.Add([]byte{0xff, 0x00})

	f.Fuzz(func(t *testing.T, raw []byte) {
		definition, parsed, parseErr := decodeAgentDraftProposal(raw, "fuzz-review", limits)
		if parseErr != nil {
			if !errors.Is(parseErr, ErrAgentDraft) {
				t.Fatalf("error does not preserve ErrAgentDraft: %v", parseErr)
			}

			return
		}
		if parsed.ID != "fuzz-review" {
			t.Fatalf("parsed unexpected ID %q", parsed.ID)
		}
		if int64(len(definition)) > limits.MaxDefinitionBytes {
			t.Fatalf("accepted %d definition bytes", len(definition))
		}
		definition[0] ^= 0xff
		if parsed.ID != "fuzz-review" {
			t.Fatal("definition alias mutated parsed result")
		}
	})
}

func testAgentDraftDefinition(name, instructions string) string {
	return "---\n" +
		"schema: pips.agent/v1alpha1\n" +
		"name: " + name + "\n" +
		"description: Review one bounded target.\n" +
		"model: inherit\n" +
		"visibility:\n  user: true\n  model: false\n" +
		"delivery: [foreground]\n" +
		"tools:\n  allow: [\"tool:read\"]\n" +
		"output:\n  format: text\n" +
		"---\n" + instructions + "\n"
}

func testAgentDraftProposalResponse(t *testing.T, definition string) *ai.Response {
	t.Helper()

	return runtimeToolResponse(
		"propose-draft",
		agentDraftToolName,
		mustJSON(t, agentDraftProposalRequest{Definition: definition}),
	)
}

func agentLibraryContains(library AgentLibrary, id string) bool {
	for _, entry := range library.Entries {
		if entry.ID == id {
			return true
		}
	}

	return false
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)

	return string(data)
}

func agentDraftTestLimits() agentprofile.Limits {
	return agentprofile.DefaultLimits()
}
