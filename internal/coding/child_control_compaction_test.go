//nolint:wsl_v5 // Child scope fixtures keep lifecycle ownership and assertions adjacent.
package coding

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/compaction"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/credential"
	"github.com/rsbin1178/pips/internal/coding/generation"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type childCompactionFixture struct {
	runtime    *Runtime
	child      *session.Handle
	factory    childScopeFactory
	plan       subagent.ExecutionPlan
	definition agentprofile.Definition
	ambient    []catalog.Descriptor
}

func newChildCompactionFixture(t *testing.T, model ai.LanguageModel, allowHistory bool) childCompactionFixture {
	t.Helper()
	r := openTestRuntime(t, model)
	child, err := r.repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: r.workspace.Identity().Key(), WorkspacePath: r.workspace.Root(),
		Kind: session.KindSubagent, ParentSessionID: r.handle.Metadata().ID,
		ParentRunID: "parent-run", Agent: "checkpoint-child",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, child.Close()) })
	definition := agentprofile.Definition{
		ID: "checkpoint-child", Kind: agentprofile.KindCustom, Scope: agentprofile.ScopeUserPips,
		Source: "user:pips/checkpoint-child.md", Digest: strings.Repeat("a", 64),
		Schema: agentprofile.SchemaV1Alpha1, Name: "Checkpoint child", Model: "inherit",
	}
	identity, err := subagent.IdentityFromDefinition(definition)
	require.NoError(t, err)
	output, err := subagent.NewOutputContract(subagent.OutputFormatJSONSchema, "child-result", ai.JSON(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`))
	require.NoError(t, err)
	limits := subagent.DefaultLimits()
	limits.MaxOutputTokens = 64
	instructions := "Inspect only the delegated file. Preserve the child assignment and return the required JSON result."
	ambient := []catalog.Descriptor{
		{Name: "read", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskRead},
		{Name: tools.HistoryName, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.history"}, Risk: catalog.RiskRead, Tags: []string{"history", "read-only"}},
	}
	capabilities := []subagent.EffectiveCapability{{WireName: "read", Source: "local/coding", Risk: "read"}}
	if allowHistory {
		capabilities = append(capabilities, subagent.EffectiveCapability{WireName: tools.HistoryName, Source: "local/coding.history", Risk: "read"})
	}
	plan := subagent.ExecutionPlan{
		Schema: subagent.ExecutionPlanSchema, Identity: identity, GenerationID: r.integration.ID(),
		Delivery: subagent.DeliveryForeground, Model: "openai/runtime-test",
		Instructions: instructions, InstructionsDigest: subagent.InstructionsDigest(instructions),
		Capabilities: capabilities, Skills: []string{}, PreloadedSkills: []string{},
		Limits: limits, Output: output,
	}
	require.NoError(t, subagent.ValidateExecutionPlan(plan))
	factory := childScopeFactory{
		repository:     r.repository,
		fullCompaction: &compaction.Policy{ContextWindow: 8_000, ReserveTokens: 512, MinSummaryChars: 1, AttemptsPerStage: 1},
		workspace:      r.workspace, tree: r.tree, toolLimits: r.opts.ToolLimits,
		policy: r.policy, executor: r.executor, inspector: r.inspector,
		sandbox: r.config.Sandbox, network: r.config.SandboxWorkspaceWrite.Network,
		model: model, controls: r.childControls, mode: ModeAgent, toolTimeout: r.opts.ToolTimeout,
		requestPolicy: func(request *ai.Request) { request.MaxTokens = ai.Ptr(1024) },
	}
	return childCompactionFixture{r, child, factory, plan, definition, ambient}
}

func (f childCompactionFixture) open(t *testing.T) *childControlScope {
	t.Helper()
	scope, err := newChildControlScope(t.Context(), f.factory, f.runtime.integration, f.ambient, nil, subagent.DispatchInput{
		Plan: f.plan, Child: f.child, OnEvent: func(context.Context, agent.Event) {},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, scope.Close(context.Background())) })
	return scope
}

func highUsageChildToolResponse() *ai.Response {
	response := runtimeToolResponse("child-read", "read", `{"path":"target.txt"}`)
	response.Usage.InputTokens = 9_000
	return response
}

func TestChildScopeFullCompactionCommitsArchiveAndRebuildsContext(t *testing.T) {
	t.Parallel()
	model := &childCompactionModel{runtimeModel: newRuntimeModel(
		highUsageChildToolResponse(), runtimeTextResponse("<summary>The file was inspected; finish the assigned report.</summary>"),
		runtimeTextResponse(`{"summary":"done"}`),
	)}
	fixture := newChildCompactionFixture(t, model, true)
	require.NoError(t, os.WriteFile(filepath.Join(fixture.runtime.workspace.Root(), "target.txt"), []byte("child-only evidence\n"), 0o600))
	parentID := appendChildTestArchive(t, fixture.runtime.repository, fixture.runtime.handle, "parent-only evidence")
	sibling, err := fixture.runtime.repository.Create(t.Context(), session.CreateOptions{WorkspaceID: fixture.runtime.workspace.Identity().Key(), WorkspacePath: fixture.runtime.workspace.Root()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sibling.Close()) })
	siblingID := appendChildTestArchive(t, fixture.runtime.repository, sibling, "sibling-only evidence")
	parentStore, err := fixture.runtime.repository.Archives(fixture.runtime.handle)
	require.NoError(t, err)
	parentBound := tools.NewHistoryTool(func(context.Context) (*session.ArchiveReader, error) { return parentStore.Bind([]string{parentID}) })
	fixture.factory.mcpEntries = catalog.Local("coding.history", catalog.RiskRead, parentBound)
	scope := fixture.open(t)
	result, err := scope.Run(t.Context(), "Inspect target.txt and finish the report.")
	require.NoError(t, err)
	assert.JSONEq(t, `{"summary":"done"}`, result.Text)
	assert.Equal(t, 1, result.ToolCalls)
	assert.Equal(t, 9_010, result.Run.Usage.InputTokens, "summary usage is not charged to the ordinary run")
	assert.Equal(t, fixture.plan.Limits, scope.plan.Limits)

	ids := compaction.ArchiveIDs(fixture.child.Session().Path())
	require.Len(t, ids, 1)
	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.Contains(t, toolNamesFromRequest(requests[0]), tools.HistoryName)
	require.NotNil(t, requests[0].ResponseFormat)
	assert.Nil(t, requests[1].ResponseFormat, "summary must not inherit the child final-output schema")
	assert.Empty(t, requests[1].Tools)
	assert.Equal(t, 1024, *requests[1].MaxTokens, "summary uses factory policy, not child output clamp")
	assert.Equal(t, 64, *requests[2].MaxTokens)
	require.NotNil(t, requests[2].ResponseFormat)
	assert.Equal(t, requests[0].Messages[0], requests[2].Messages[0], "authoritative instructions are rebuilt by the same owner")
	for _, message := range requests[2].Messages {
		_, isTool := message.(ai.ToolMessage)
		assert.False(t, isTool)
		assert.NotContains(t, childMessageText(message), "child-only evidence", "old tool tail is not reintroduced")
	}
	assert.Contains(t, childMessageText(requests[2].Messages[1]), ids[0])
	assert.Equal(t, ai.UserText("Inspect target.txt and finish the report."), requests[2].Messages[2])
	assert.Equal(t, fixture.plan.Output.Schema, requests[2].ResponseFormat.Schema.RawJSON)

	historyCatalog, err := scope.historyCatalog(fixture.ambient)
	require.NoError(t, err)
	historyTools, err := historyCatalog.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskRead))
	require.NoError(t, err)
	require.Len(t, historyTools, 1)
	history := historyTools[0]
	parts, err := history.Exec(t.Context(), agent.ToolCall{Args: ai.JSON(`{"action":"list"}`)})
	require.NoError(t, err)
	var listed session.ArchiveListPage
	require.NoError(t, json.Unmarshal([]byte(historyText(t, parts)), &listed))
	require.Len(t, listed.Archives, 1)
	assert.Equal(t, ids[0], listed.Archives[0].ID)
	assert.Equal(t, fixture.child.Metadata().ID, listed.Archives[0].Source.SessionID)
	for _, id := range []string{parentID, siblingID} {
		args, marshalErr := json.Marshal(map[string]any{"action": "search", "archive_id": id, "query": "evidence"})
		require.NoError(t, marshalErr)
		_, callErr := history.Exec(t.Context(), agent.ToolCall{Args: args})
		require.ErrorIs(t, callErr, session.ErrArchiveDenied)
	}
	args, err := json.Marshal(map[string]any{"action": "search", "archive_id": ids[0], "query": "child-only evidence"})
	require.NoError(t, err)
	parts, err = history.Exec(t.Context(), agent.ToolCall{Args: args})
	require.NoError(t, err)
	assert.Contains(t, historyText(t, parts), "child-only evidence", "archive contains the durably flushed Tool result")
}

func TestChildScopeCompactionFailsClosedWithoutProfileHistory(t *testing.T) {
	t.Parallel()
	model := &childCompactionModel{runtimeModel: newRuntimeModel(highUsageChildToolResponse())}
	fixture := newChildCompactionFixture(t, model, false)
	require.NoError(t, os.WriteFile(filepath.Join(fixture.runtime.workspace.Root(), "target.txt"), []byte("evidence"), 0o600))
	scope := fixture.open(t)
	_, err := scope.Run(t.Context(), "Inspect target.txt.")
	require.ErrorIs(t, err, compaction.ErrBudget)
	assert.Contains(t, err.Error(), "profile permission for session_history")
	assert.Empty(t, compaction.ArchiveIDs(fixture.child.Session().Path()))
	require.Len(t, model.Requests(), 1, "no summary or over-budget continuation is sampled")
	assert.NotContains(t, toolNamesFromRequest(model.Requests()[0]), tools.HistoryName)
}

func TestChildScopeCompactionSummaryFailureAndCancellationLeaveHistory(t *testing.T) {
	t.Parallel()
	for _, cancelSummary := range []bool{false, true} {
		name := "summary_failure"
		if cancelSummary {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			model := &childCompactionModel{runtimeModel: newRuntimeModel(highUsageChildToolResponse(), runtimeTextResponse(""))}
			if cancelSummary {
				model.cancelSummary = cancel
			}
			fixture := newChildCompactionFixture(t, model, true)
			require.NoError(t, os.WriteFile(filepath.Join(fixture.runtime.workspace.Root(), "target.txt"), []byte("retained evidence"), 0o600))
			scope := fixture.open(t)
			_, err := scope.Run(ctx, "Inspect target.txt.")
			if cancelSummary {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, compaction.ErrSummary)
			}
			assert.Empty(t, compaction.ArchiveIDs(fixture.child.Session().Path()))
			contextSnapshot, contextErr := fixture.child.Session().Context()
			require.NoError(t, contextErr)
			require.Len(t, contextSnapshot.Messages, 3)
			retained, marshalErr := json.Marshal(contextSnapshot.Messages[2])
			require.NoError(t, marshalErr)
			assert.Contains(t, string(retained), "retained evidence")
			require.Len(t, model.Requests(), 2)
		})
	}
}

func TestChildScopeDispatcherUsesSelectedModelWindowAndClonesPolicy(t *testing.T) {
	t.Parallel()
	for _, window := range []int{4096, 0} {
		name := "smaller_window"
		if window == 0 {
			name = "unknown_window"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			baseModel := newRuntimeModel()
			fixture := newChildCompactionFixture(t, baseModel, true)
			baseRef := config.ModelRef{Provider: ai.ProviderOpenAI, Model: "runtime-test"}
			childRef := config.ModelRef{Provider: ai.ProviderOpenAI, Model: "smaller-child"}
			cfg := config.Defaults()
			cfg.Model = baseRef
			cfg.Providers[ai.ProviderOpenAI] = config.ProviderConfig{BaseURL: "https://api.openai.com/v1", Protocol: config.ProtocolOpenAIResponses}
			cfg.Models = []config.ModelConfig{{Ref: baseRef, ContextWindow: 128_000}, {Ref: childRef, ContextWindow: window}}
			models, err := modelcatalog.New(cfg)
			require.NoError(t, err)
			base, err := models.Resolve(modelcatalog.Selection{Ref: baseRef})
			require.NoError(t, err)
			policy, err := generation.Compile(base)
			require.NoError(t, err)
			credentials, err := credential.NewEnvironmentStore(func(string) (string, bool) { return "test-key", true })
			require.NoError(t, err)
			resolver, err := newChildModelResolver(models, base, baseModel, policy, credentials, func(bound ai.LanguageModel) ai.LanguageModel {
				return newRuntimeModelFor(bound.Provider(), bound.ModelID())
			})
			require.NoError(t, err)
			inherited, err := resolver.bind(t.Context(), baseRef.String())
			require.NoError(t, err)
			assert.Equal(t, 128_000, inherited.contextWindow)
			fixture.definition.Model = childRef.String()
			fixture.plan.Model = childRef.String()
			fixture.factory.fullCompaction.ContextWindow = 128_000
			dispatcher := &customSubagentDispatcher{
				definitions: map[string]agentprofile.Definition{fixture.definition.ID: fixture.definition},
				models:      resolver, generation: fixture.runtime.integration, factory: fixture.factory, ambient: fixture.ambient,
			}
			runner, err := dispatcher.Open(t.Context(), subagent.DispatchInput{Plan: fixture.plan, Child: fixture.child, OnEvent: func(context.Context, agent.Event) {}})
			if window == 0 {
				require.ErrorIs(t, err, compaction.ErrInvalid, "unknown window must not silently inherit parent capacity")
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, runner.Close(context.Background())) })
			scope, ok := runner.(*childControlScope)
			require.True(t, ok)
			assert.Equal(t, window, scope.factory.fullCompaction.ContextWindow)
			assert.Equal(t, childRef.Model, scope.factory.model.ModelID())
			scope.factory.fullCompaction.ReserveTokens++
			assert.Equal(t, 128_000, fixture.factory.fullCompaction.ContextWindow)
			assert.Equal(t, 512, fixture.factory.fullCompaction.ReserveTokens)
		})
	}
}

func appendChildTestArchive(t *testing.T, repository *session.Repository, handle *session.Handle, evidence string) string {
	t.Helper()
	_, err := handle.Session().AppendMessage(ai.UserText(evidence), nil)
	require.NoError(t, err)
	_, err = handle.Session().AppendMessage(ai.AssistantText("prior result"), nil)
	require.NoError(t, err)
	store, err := repository.Archives(handle)
	require.NoError(t, err)
	tip := handle.Session().LeafID()
	staged, err := store.Stage(t.Context(), session.ArchiveSource{SessionID: handle.Metadata().ID, TipID: tip}, handle.Session().Path())
	require.NoError(t, err)
	require.NoError(t, staged.Publish(t.Context()))
	require.NoError(t, staged.Close())
	_, err = handle.Session().AppendContextCheckpoint(tip, harness.ContextCheckpoint{Version: harness.ContextCheckpointVersion, ArchiveID: staged.ID(), Messages: ai.Messages{ai.UserText("prior checkpoint")}}, 100)
	require.NoError(t, err)
	return staged.ID()
}

func historyText(t *testing.T, parts []ai.Part) string {
	t.Helper()
	require.Len(t, parts, 1)
	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)
	return text.Text
}

type childCompactionModel struct {
	*runtimeModel
	cancelSummary context.CancelFunc
}

func (m *childCompactionModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	if len(request.Tools) == 0 && m.cancelSummary != nil {
		m.mu.Lock()
		m.requests = append(m.requests, request)
		m.mu.Unlock()
		m.cancelSummary()
		return nil, ctx.Err()
	}
	return m.runtimeModel.Generate(ctx, request)
}

func (m *childCompactionModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
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

func (*childCompactionModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}
