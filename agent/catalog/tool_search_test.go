package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewToolSearchValidatesName(t *testing.T) {
	t.Parallel()

	read := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	c, err := catalog.New(catalog.Local("app", catalog.RiskRead, read)...)
	require.NoError(t, err)

	policy := catalog.AllowAll("test", catalog.RiskRead)

	search, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	assert.Equal(t, catalog.DefaultToolSearchName, search.Name())

	search, err = catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true, Name: "find-Tools_2"})
	require.NoError(t, err)
	assert.Equal(t, "find-Tools_2", search.Name())

	for _, invalid := range []string{"bad name", "tool.search", strings.Repeat("a", 65), "tool/search"} {
		_, err = catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true, Name: invalid})
		require.Error(t, err, invalid)
		require.Error(t, catalog.ValidateToolSearchName(invalid), invalid)
	}

	_, err = catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true, Name: "read_doc"})
	require.ErrorContains(t, err, `"read_doc" is reserved`)

	initial := agent.NewTool("status", "", func(context.Context, struct{}) (string, error) { return "", nil })
	_, err = catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{
		Enabled: true, Name: "status", Initial: []agent.Tool{initial},
	})
	require.ErrorContains(t, err, `duplicate initial tool "status"`)
}

func TestToolSearchIsHiddenWithoutDeferredTools(t *testing.T) {
	t.Parallel()

	read := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	lookup := agent.NewTool("lookup_docs", "Look up documentation", func(context.Context, struct{}) (string, error) { return "", nil })
	policy := catalog.AllowAll("test", catalog.RiskRead)

	localOnly, err := catalog.New(catalog.Local("app", catalog.RiskRead, read)...)
	require.NoError(t, err)
	search, err := catalog.NewToolSearch(localOnly, policy, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	tools, err := search.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc"}, toolNames(tools))
	sources, err := search.DeferredSources(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sources)

	// The executable set keeps a hidden copy so a blind call is answered.
	executable, err := search.ExecutableTools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc", catalog.DefaultToolSearchName}, toolNames(executable))
	assert.True(t, executable[1].Decl().Disabled)

	withMCP, err := catalog.New(
		catalog.Local("app", catalog.RiskRead, read)[0],
		catalog.MCP("docs", catalog.RiskRead, lookup)[0],
	)
	require.NoError(t, err)
	search, err = catalog.NewToolSearch(withMCP, policy, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	tools, err = search.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc", catalog.DefaultToolSearchName}, toolNames(tools))
	assert.False(t, tools[1].Decl().Disabled)
	executable, err = search.ExecutableTools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, toolNames(tools), toolNames(executable))
	sources, err = search.DeferredSources(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []catalog.SourceSummary{{Kind: catalog.SourceMCP, ID: "docs", Tools: 1}}, sources)

	// A deferred tool the policy does not authorize does not surface the tool.
	denied := catalog.Policy{TenantID: "test", Allowlist: []string{"read_doc"}, MaxRisk: catalog.RiskRead}
	search, err = catalog.NewToolSearch(withMCP, denied, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	tools, err = search.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc"}, toolNames(tools))

	// Disabled search never reports discoverable sources.
	search, err = catalog.NewToolSearch(withMCP, policy, catalog.ToolSearchOptions{})
	require.NoError(t, err)
	sources, err = search.DeferredSources(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestToolSearchReportsSourcesAndHints(t *testing.T) {
	t.Parallel()

	read := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	issues := agent.NewTool("list_issues", "List GitHub issues", func(context.Context, struct{}) (string, error) { return "", nil })
	pulls := agent.NewTool("list_pulls", "List GitHub pull requests", func(context.Context, struct{}) (string, error) { return "", nil })
	post := agent.NewTool("post_message", "Post a Slack message", func(context.Context, struct{}) (string, error) { return "", nil })
	policy := catalog.AllowAll("test", catalog.RiskRead)

	t.Run("nothing deferred", func(t *testing.T) {
		t.Parallel()

		c, err := catalog.New(catalog.Local("app", catalog.RiskRead, read)...)
		require.NoError(t, err)
		search, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true})
		require.NoError(t, err)

		result := runToolSearch(t, search, `{"query":"web search tool"}`)
		assert.JSONEq(
			t,
			`{"matches":[],"activated":[],"sources":[],"hint":"No MCP or Extension tools are registered in this session; nothing can be discovered."}`,
			result,
		)

		// The hint answers even an argument-less probe.
		assert.Contains(t, runToolSearch(t, search, `{}`), "nothing can be discovered")
	})

	t.Run("miss lists sources", func(t *testing.T) {
		t.Parallel()

		c, err := catalog.New(
			catalog.Local("app", catalog.RiskRead, read)[0],
			catalog.MCP("github", catalog.RiskRead, issues)[0],
			catalog.MCP("github", catalog.RiskRead, pulls)[0],
			catalog.MCP("slack", catalog.RiskRead, post)[0],
		)
		require.NoError(t, err)
		search, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true})
		require.NoError(t, err)

		type decodedResult struct {
			Matches   []json.RawMessage       `json:"matches"`
			Activated []string                `json:"activated"`
			Sources   []catalog.SourceSummary `json:"sources"`
			Hint      string                  `json:"hint"`
		}

		var decoded decodedResult
		require.NoError(t, json.Unmarshal([]byte(runToolSearch(t, search, `{"query":"deploy kubernetes"}`)), &decoded))
		assert.Empty(t, decoded.Matches)
		assert.Empty(t, decoded.Activated)
		assert.Equal(t, []catalog.SourceSummary{
			{Kind: catalog.SourceMCP, ID: "github", Tools: 2},
			{Kind: catalog.SourceMCP, ID: "slack", Tools: 1},
		}, decoded.Sources)
		assert.Equal(
			t,
			"No deferred tool matched. Searchable sources: mcp/github (2), mcp/slack (1). Try broader or different terms.",
			decoded.Hint,
		)

		var hit decodedResult
		require.NoError(t, json.Unmarshal([]byte(runToolSearch(t, search, `{"query":"github issues"}`)), &hit))
		assert.Empty(t, hit.Hint)
		assert.Equal(t, []string{"list_issues", "list_pulls"}, hit.Activated)
		assert.Len(t, hit.Sources, 2)
	})
}

func TestToolSearchDispatchReachesHiddenToolsAndRefusesVisibleOnes(t *testing.T) {
	t.Parallel()

	local := newRecordingCatalogTool("read", "local result")
	remote := newRecordingCatalogTool("docs__lookup", "remote result")
	broken := newRecordingCatalogTool("docs__broken", "")
	broken.failure = errors.New("target failure")
	c, err := catalog.New(
		catalog.Local("app", catalog.RiskRead, local)[0],
		catalog.MCP("docs", catalog.RiskRead, remote)[0],
		catalog.MCP("docs", catalog.RiskRead, broken)[0],
	)
	require.NoError(t, err)

	tool := &testDispatchTool{}
	search, err := catalog.NewToolSearch(c, catalog.AllowAll("test", catalog.RiskRead), catalog.ToolSearchOptions{
		Enabled: true, UseTool: dispatchToolBuilder(tool),
	})
	require.NoError(t, err)

	// The dispatch Tool is part of every snapshot this surface reports, and the
	// hidden targets are not.
	visible, err := search.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read", catalog.DefaultToolSearchName, "use_tool"}, toolNames(visible))

	// A deferred tool the snapshot hides is reached with the caller's arguments,
	// and its own result is what comes back.
	parts, err := search.Dispatch(t.Context(), "docs__lookup", ai.JSON(`{"query":"parity"}`))
	require.NoError(t, err)
	assert.Equal(t, agent.TextResult("remote result"), parts)
	assert.Equal(t, []string{`{"query":"parity"}`}, remote.calls)

	// A target's own failure is returned as it is, not restated.
	_, err = search.Dispatch(t.Context(), "docs__broken", ai.JSON(`{}`))
	require.EqualError(t, err, "target failure")

	// A directly visible tool is refused with the correction and never runs.
	_, err = search.Dispatch(t.Context(), "read", ai.JSON(`{}`))
	require.EqualError(t, err,
		`tool "read" is directly visible in this session; `+
			"call it as its own tool call instead of routing it through use_tool")
	assert.Empty(t, local.calls)

	// An activated deferred tool is part of the snapshot too, so the wrapper
	// refuses it as well: it carries only what the snapshot hides.
	activations := catalog.NewActivationSet(catalog.DefaultActivationLimit)
	activated := &testDispatchTool{}
	activatedSearch, err := catalog.NewToolSearch(c, catalog.AllowAll("test", catalog.RiskRead), catalog.ToolSearchOptions{
		Enabled: true, UseTool: dispatchToolBuilder(activated), Activations: activations,
	})
	require.NoError(t, err)

	activations.Add("docs__lookup")

	_, err = activatedSearch.Dispatch(t.Context(), "docs__lookup", ai.JSON(`{}`))
	require.ErrorContains(t, err, `tool "docs__lookup" is directly visible`)
	assert.Len(t, remote.calls, 1, "an activated target is never dispatched")
}

func TestToolSearchDispatchRefusesUnknownAndUnauthorizedNames(t *testing.T) {
	t.Parallel()

	remote := newRecordingCatalogTool("docs__lookup", "remote result")
	c, err := catalog.New(catalog.MCP("docs", catalog.RiskRead, remote)...)
	require.NoError(t, err)

	tool := &testDispatchTool{}
	search, err := catalog.NewToolSearch(c, catalog.AllowAll("test", catalog.RiskRead), catalog.ToolSearchOptions{
		Enabled: true, UseTool: dispatchToolBuilder(tool),
	})
	require.NoError(t, err)

	_, err = search.Dispatch(t.Context(), "docs__missing", ai.JSON(`{}`))
	require.ErrorContains(t, err, `unknown tool "docs__missing"`)

	// The policy that authorizes the target is the caller's, so a denied name is
	// refused by name instead of being dispatched.
	denied := &testDispatchTool{}
	restricted, err := catalog.NewToolSearch(c, catalog.Policy{Allowlist: []string{"other"}, MaxRisk: catalog.RiskRead}, catalog.ToolSearchOptions{
		Enabled: true, UseTool: dispatchToolBuilder(denied),
	})
	require.NoError(t, err)

	_, err = restricted.Dispatch(t.Context(), "docs__lookup", ai.JSON(`{}`))
	require.ErrorContains(t, err, `tool "docs__lookup" is not authorized`)
	assert.Empty(t, remote.calls)
}

func TestToolSearchOffersDispatchToolOnlyWhenEnabledAndBuilt(t *testing.T) {
	t.Parallel()

	local := newRecordingCatalogTool("read", "local result")
	c, err := catalog.New(catalog.Local("app", catalog.RiskRead, local)...)
	require.NoError(t, err)

	policy := catalog.AllowAll("test", catalog.RiskRead)

	// A surface built without a dispatch tool offers none, so a call by name is
	// refused rather than reaching the catalog.
	withoutTool, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	visible, err := withoutTool.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read"}, toolNames(visible))
	_, err = withoutTool.Dispatch(t.Context(), "read", ai.JSON(`{}`))
	require.ErrorContains(t, err, "requires an enabled search surface")

	// A disabled surface never builds one, even when the builder is injected.
	disabled := &testDispatchTool{}
	off, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{
		Enabled: false, UseTool: dispatchToolBuilder(disabled),
	})
	require.NoError(t, err)
	visible, err = off.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read"}, toolNames(visible))
	assert.Nil(t, disabled.dispatch)
}

func TestToolSearchRejectsADispatchToolNameCollision(t *testing.T) {
	t.Parallel()

	local := newRecordingCatalogTool("read", "local result")
	c, err := catalog.New(catalog.Local("app", catalog.RiskRead, local)...)
	require.NoError(t, err)

	policy := catalog.AllowAll("test", catalog.RiskRead)

	named := func(name string) func(catalog.UseToolDispatch) agent.Tool {
		return func(catalog.UseToolDispatch) agent.Tool {
			return agent.NewTool(name, "", func(context.Context, struct{}) (string, error) { return "", nil })
		}
	}

	for _, test := range []struct {
		name    string
		builder func(catalog.UseToolDispatch) agent.Tool
		want    string
	}{
		{name: "discovery name", builder: named(catalog.DefaultToolSearchName), want: "reserved for deferred tool discovery"},
		{name: "searchable name", builder: named("read"), want: `use tool "read" duplicates a searchable tool`},
		{name: "empty name", builder: named(""), want: "use tool has an empty name"},
		{
			name:    "nil tool",
			builder: func(catalog.UseToolDispatch) agent.Tool { return nil },
			want:    "use tool builder returned nil",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{
				Enabled: true, UseTool: test.builder,
			})
			require.ErrorContains(t, err, test.want)
		})
	}
}

// runToolSearch runs one scripted agent turn that calls the discovery tool by
// name and returns the tool result text the model would see. AgentOptions
// installs ExecutableTools, so the call succeeds even when the tool is hidden.
func runToolSearch(t *testing.T, search *catalog.ToolSearch, args string) string {
	t.Helper()

	opts, err := search.AgentOptions(t.Context())
	require.NoError(t, err)

	model := &generateOnlyModel{responses: []*ai.Response{
		{
			Message:      ai.Assistant(ai.ToolCallPart{ID: "call-1", Name: search.Name(), Args: ai.JSON(args)}),
			FinishReason: ai.FinishToolCalls,
		},
		{Message: ai.AssistantText("done"), FinishReason: ai.FinishStop},
	}}
	a, err := agent.New(model, opts...)
	require.NoError(t, err)

	sess := agent.NewSession()
	_, err = a.Run(t.Context(), sess, ai.UserText("find tools"))
	require.NoError(t, err)

	for _, message := range sess.Messages() {
		toolMessage, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}

		for _, part := range toolMessage.Parts {
			if part.ToolCallID != "call-1" {
				continue
			}

			require.False(t, part.IsError, "tool result: %v", part.Content)
			require.Len(t, part.Content, 1)
			text, ok := part.Content[0].(ai.TextPart)
			require.True(t, ok)

			return text.Text
		}
	}

	t.Fatal("tool result not found")

	return ""
}

type generateOnlyModel struct {
	responses []*ai.Response
	position  int
}

func (m *generateOnlyModel) Generate(_ context.Context, _ ai.Request) (*ai.Response, error) {
	if m.position >= len(m.responses) {
		return nil, errors.New("script exhausted")
	}

	response := m.responses[m.position]
	m.position++

	return response, nil
}

func (m *generateOnlyModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errors.New("streaming is not scripted"))
	}
}

func (m *generateOnlyModel) Provider() ai.Provider { return ai.Provider("scripted") }
func (m *generateOnlyModel) ModelID() string       { return "scripted" }
func (m *generateOnlyModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func toolNames(tools []agent.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Decl().Name)
	}

	return names
}

// recordingCatalogTool is one registered tool that answers with a fixed result
// and keeps the raw arguments it was called with, so a dispatch can be shown to
// hand the caller's object over unchanged.
type recordingCatalogTool struct {
	name    string
	result  string
	failure error
	calls   []string
}

func newRecordingCatalogTool(name, result string) *recordingCatalogTool {
	return &recordingCatalogTool{name: name, result: result}
}

func (t *recordingCatalogTool) Decl() ai.Tool {
	return ai.Tool{Name: t.name, Description: t.name}
}

func (t *recordingCatalogTool) Exec(_ context.Context, call agent.ToolCall) ([]ai.Part, error) {
	t.calls = append(t.calls, string(call.Args))
	if t.failure != nil {
		return nil, t.failure
	}

	return agent.TextResult(t.result), nil
}

// testDispatchTool is a minimal dispatch Tool: it forwards the wrapper's two
// arguments to the surface's own dispatcher.
type testDispatchTool struct {
	dispatch catalog.UseToolDispatch
}

func (t *testDispatchTool) Decl() ai.Tool {
	return ai.Tool{Name: "use_tool", Description: "Call a hidden tool by name."}
}

func (t *testDispatchTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	var args struct {
		ToolName  string  `json:"tool_name"`
		ToolInput ai.JSON `json:"tool_input"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return nil, err
	}

	return t.dispatch(ctx, args.ToolName, args.ToolInput)
}

// dispatchToolBuilder wires the surface's dispatcher into one testDispatchTool.
func dispatchToolBuilder(tool *testDispatchTool) func(catalog.UseToolDispatch) agent.Tool {
	return func(dispatch catalog.UseToolDispatch) agent.Tool {
		tool.dispatch = dispatch

		return tool
	}
}
