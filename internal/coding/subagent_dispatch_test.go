package coding

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/hooks"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileDelegableCapabilitiesIsAnAmbientSubset(t *testing.T) {
	t.Parallel()

	ambient := []catalog.Descriptor{
		{
			Name: "read", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
			Risk: catalog.RiskRead, Tags: []string{"builtin", "filesystem"},
		},
		{
			Name: "shell", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
			Risk: catalog.RiskPrivileged, Tags: []string{"builtin", "process"},
		},
		{
			Name: "spawn_subagent", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"},
			Risk: catalog.RiskRead, Tags: []string{"builtin"},
		},
		{
			Name: "write_plan", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.plan"},
			Risk: catalog.RiskWrite, Tags: []string{"builtin"},
		},
	}

	capabilities, err := compileDelegableCapabilities(agentprofile.ToolSelection{
		Allow: []agentprofile.Selector{
			{Kind: agentprofile.SelectorTool, Value: "read"},
			{Kind: agentprofile.SelectorTag, Value: "process"},
		},
		Require: []agentprofile.Selector{{Kind: agentprofile.SelectorTool, Value: "shell"}},
	}, "", ambient)
	require.NoError(t, err)
	assert.Equal(t, []subagent.EffectiveCapability{
		{WireName: "read", Source: "local/coding", Risk: "read"},
		{WireName: "shell", Source: "local/coding", Risk: "privileged"},
	}, capabilities)

	_, err = compileDelegableCapabilities(agentprofile.ToolSelection{
		Allow:   []agentprofile.Selector{{Kind: agentprofile.SelectorTool, Value: "spawn_subagent"}},
		Require: []agentprofile.Selector{{Kind: agentprofile.SelectorTool, Value: "spawn_subagent"}},
	}, "", ambient)
	require.ErrorIs(t, err, subagent.ErrInvalid)
	assert.Contains(t, err.Error(), "required capability")
}

func TestCompileDelegableCapabilitiesProperties(t *testing.T) {
	t.Parallel()

	ambient := []catalog.Descriptor{
		{Name: "read", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskRead, Tags: []string{"filesystem"}},
		{Name: "Edit", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskWrite, Tags: []string{"filesystem"}},
		{Name: "shell", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskPrivileged, Tags: []string{"process"}},
		{Name: "search", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "github"}, Risk: catalog.RiskRead, Tags: []string{"remote"}},
		{Name: "update_issue", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "github"}, Risk: catalog.RiskWrite, Tags: []string{"remote"}},
		{Name: "admin", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "github"}, Risk: catalog.RiskPrivileged, Tags: []string{"remote"}},
		{Name: "ask_user", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.question"}, Risk: catalog.RiskRead, Tags: []string{"input"}},
		{Name: harness.SkillToolName, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.skills"}, Risk: catalog.RiskRead, Tags: []string{"skills"}},
		{Name: "spawn_subagent", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"}, Risk: catalog.RiskRead, Tags: []string{"delegation"}},
		{Name: "write_plan", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.plan"}, Risk: catalog.RiskWrite, Tags: []string{"planning"}},
	}
	selectors := []agentprofile.Selector{
		{Kind: agentprofile.SelectorTool, Value: "read"},
		{Kind: agentprofile.SelectorTool, Value: "Edit"},
		{Kind: agentprofile.SelectorSource, Value: "mcp/github"},
		{Kind: agentprofile.SelectorTag, Value: "process"},
	}

	compiled := make(map[int][]subagent.EffectiveCapability, 1<<len(selectors))
	for mask := range 1 << len(selectors) {
		allow := make([]agentprofile.Selector, 0, len(selectors))
		for index, selector := range selectors {
			if mask&(1<<index) != 0 {
				allow = append(allow, selector)
			}
		}

		capabilities, err := compileDelegableCapabilities(agentprofile.ToolSelection{Allow: allow}, "", ambient)
		require.NoError(t, err)

		compiled[mask] = capabilities

		for _, capability := range capabilities {
			matched := false

			for _, descriptor := range ambient {
				if descriptor.Name == capability.WireName &&
					capabilitySource(descriptor.Source) == capability.Source &&
					capabilityRisk(descriptor.Risk) == capability.Risk &&
					delegableDescriptor(descriptor) {
					matched = true
					break
				}
			}

			assert.True(t, matched, "compiled capability must be an exact ambient delegable descriptor: %+v", capability)
		}
	}

	for broadMask, broad := range compiled {
		for narrowMask, narrow := range compiled {
			if narrowMask&broadMask != narrowMask {
				continue
			}

			for _, capability := range narrow {
				assert.Containsf(
					t, broad, capability,
					"shrinking allow selectors from mask %04b to %04b added a capability",
					broadMask, narrowMask,
				)
			}
		}
	}
}

func TestCompileDelegableCapabilitiesRejectsNonDelegableInjection(t *testing.T) {
	t.Parallel()

	selection := agentprofile.ToolSelection{Allow: []agentprofile.Selector{{
		Kind: agentprofile.SelectorTag, Value: "selected",
	}}}
	ambient := []catalog.Descriptor{{
		Name: "read", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
		Risk: catalog.RiskRead, Tags: []string{"selected"},
	}}
	baseline, err := compileDelegableCapabilities(selection, "", ambient)
	require.NoError(t, err)

	injected := append(slices.Clone(ambient),
		catalog.Descriptor{
			Name: "spawn_subagent", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"},
			Risk: catalog.RiskRead, Tags: []string{"selected"},
		},
		catalog.Descriptor{
			Name: "extension_tool", Source: catalog.Source{Kind: catalog.SourceExtension, ID: "untrusted"},
			Risk: catalog.RiskPrivileged, Tags: []string{"selected"},
		},
		catalog.Descriptor{
			Name: "unknown_source", Source: catalog.Source{Kind: "unknown", ID: "source"},
			Risk: catalog.RiskRead, Tags: []string{"selected"},
		},
		catalog.Descriptor{
			Name: "unknown_risk", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "server"},
			Risk: catalog.RiskPrivileged + 1, Tags: []string{"selected"},
		},
	)

	capabilities, err := compileDelegableCapabilities(selection, "", injected)
	require.NoError(t, err)
	assert.Equal(t, baseline, capabilities)
}

func TestCompileDelegableCapabilitiesRequireFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		selector   agentprofile.Selector
		descriptor catalog.Descriptor
	}{
		{
			name:     "delegation tool",
			selector: agentprofile.Selector{Kind: agentprofile.SelectorTool, Value: "spawn_subagent"},
			descriptor: catalog.Descriptor{
				Name: "spawn_subagent", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"},
				Risk: catalog.RiskRead,
			},
		},
		{
			name:     "same tool from non-delegable source",
			selector: agentprofile.Selector{Kind: agentprofile.SelectorTool, Value: "shell"},
			descriptor: catalog.Descriptor{
				Name: "shell", Source: catalog.Source{Kind: catalog.SourceExtension, ID: "other"},
				Risk: catalog.RiskPrivileged,
			},
		},
		{
			name:     "source identity mismatch",
			selector: agentprofile.Selector{Kind: agentprofile.SelectorSource, Value: "mcp/github"},
			descriptor: catalog.Descriptor{
				Name: "search", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "gitlab"},
				Risk: catalog.RiskRead,
			},
		},
		{
			name:     "unknown risk",
			selector: agentprofile.Selector{Kind: agentprofile.SelectorTool, Value: "search"},
			descriptor: catalog.Descriptor{
				Name: "search", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "github"},
				Risk: catalog.RiskPrivileged + 1,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			selection := agentprofile.ToolSelection{
				Allow: []agentprofile.Selector{test.selector}, Require: []agentprofile.Selector{test.selector},
			}
			_, err := compileDelegableCapabilities(selection, "", []catalog.Descriptor{test.descriptor})
			require.ErrorIs(t, err, subagent.ErrInvalid)
			assert.Contains(t, err.Error(), fmt.Sprintf("required capability %q", test.selector.String()))
		})
	}
}

func TestCompileProfileLimitsOnlyNarrowsCeiling(t *testing.T) {
	t.Parallel()

	ceiling := boundedSubagentLimits()

	tests := []struct {
		name      string
		ceiling   subagent.Limits
		requested agentprofile.ExecutionLimits
		want      subagent.Limits
		wantError string
	}{
		{name: "inherit", ceiling: ceiling, want: subagent.NormalizeLimits(ceiling)},
		{
			name: "narrow every profile limit", ceiling: ceiling,
			requested: agentprofile.ExecutionLimits{MaxTurns: 16, MaxToolCalls: 9, MaxDuration: time.Minute},
			want: func() subagent.Limits {
				value := ceiling
				value.MaxTurns = 16
				value.MaxToolCalls = 9
				value.MaxDuration = time.Minute

				return subagent.NormalizeLimits(value)
			}(),
		},
		{
			name: "finite request narrows unlimited ceiling", ceiling: subagent.DefaultLimits(),
			requested: agentprofile.ExecutionLimits{MaxTurns: 16, MaxToolCalls: 9, MaxDuration: time.Minute},
			want: func() subagent.Limits {
				value := subagent.DefaultLimits()
				value.MaxTurns = 16
				value.MaxToolCalls = 9
				value.MaxDuration = time.Minute

				return subagent.NormalizeLimits(value)
			}(),
		},
		{name: "turn expansion", ceiling: ceiling, requested: agentprofile.ExecutionLimits{MaxTurns: ceiling.MaxTurns + 1}, wantError: "max_turns"},
		{name: "tool expansion", ceiling: ceiling, requested: agentprofile.ExecutionLimits{MaxToolCalls: ceiling.MaxToolCalls + 1}, wantError: "max_tool_calls"},
		{name: "duration expansion", ceiling: ceiling, requested: agentprofile.ExecutionLimits{MaxDuration: ceiling.MaxDuration + time.Second}, wantError: "max_duration"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := compileProfileLimits(test.ceiling, test.requested)
			if test.wantError != "" {
				require.ErrorIs(t, err, subagent.ErrInvalid)
				assert.Contains(t, err.Error(), test.wantError)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func boundedSubagentLimits() subagent.Limits {
	limits := subagent.DefaultLimits()
	limits.MaxTurns = 64
	limits.MaxTokens = 256_000
	limits.MaxToolCalls = 128
	limits.MaxDuration = 30 * time.Minute

	return limits
}

// TestRuntimeSubagentLimitsDefaultToUnlimited pins the Phase L default: a zero
// ExecutionOptions.Limits resolves to the guards-only policy, so a child has no
// turn, cumulative-token, Tool-call, or wall-clock budget.
func TestRuntimeSubagentLimitsDefaultToUnlimited(t *testing.T) {
	t.Parallel()

	limits := runtimeSubagentLimits(subagent.ExecutionOptions{})
	assert.Zero(t, limits.MaxTurns)
	assert.Zero(t, limits.MaxTokens)
	assert.Zero(t, limits.MaxToolCalls)
	assert.Zero(t, limits.MaxDuration)
	assert.Zero(t, limits.FinalizationTurns)
	assert.Equal(t, subagent.DefaultLimits().RepeatedToolCallLimit, limits.RepeatedToolCallLimit)
	assert.Equal(t, subagent.DefaultLimits().MaxActivityTools, limits.MaxActivityTools)
	assert.Equal(t, subagent.DefaultLimits().MaxOutputTokens, limits.MaxOutputTokens)

	override := subagent.DefaultLimits()
	override.MaxTurns = 3
	assert.Equal(t, 3, runtimeSubagentLimits(subagent.ExecutionOptions{Limits: override}).MaxTurns)
}

// TestResolveSubagentMaxTurnsDefinitionWinsElseInheritsParent mirrors the
// reference's subagent_max_turns_definition_wins_else_inherits_parent: the
// definition's declared cap wins, otherwise the parent's, and zero is uncapped.
func TestResolveSubagentMaxTurnsDefinitionWinsElseInheritsParent(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 2, resolveSubagentMaxTurns(2, 5))
	assert.Equal(t, 5, resolveSubagentMaxTurns(0, 5))
	assert.Zero(t, resolveSubagentMaxTurns(0, 0))

	// An unbounded ceiling is the default: a definition that declares no cap
	// leaves the child uncapped, and one that declares a cap sets it.
	uncapped, err := compileProfileLimits(subagent.DefaultLimits(), agentprofile.ExecutionLimits{})
	require.NoError(t, err)
	assert.Zero(t, uncapped.MaxTurns)
	assert.Zero(t, uncapped.FinalizationTurns)

	defined, err := compileProfileLimits(
		subagent.DefaultLimits(), agentprofile.ExecutionLimits{MaxTurns: 7},
	)
	require.NoError(t, err)
	assert.Equal(t, 7, defined.MaxTurns)
	assert.Positive(t, defined.FinalizationTurns)

	// A finite parent cap is inherited only when the definition declares none.
	inherited, err := compileProfileLimits(boundedSubagentLimits(), agentprofile.ExecutionLimits{})
	require.NoError(t, err)
	assert.Equal(t, 64, inherited.MaxTurns)
	downgraded, err := compileProfileLimits(
		boundedSubagentLimits(), agentprofile.ExecutionLimits{MaxTurns: 7},
	)
	require.NoError(t, err)
	assert.Equal(t, 7, downgraded.MaxTurns)
}

func TestCompilePrivateIntegrationsRequiresExactGenerationBindings(t *testing.T) {
	t.Parallel()

	tool := agent.NewTool("private_search", "private search", func(context.Context, struct{}) (string, error) {
		return "ok", nil
	})
	entry := catalog.Entry{
		Tool: tool, Source: catalog.Source{Kind: catalog.SourceMCP, ID: "private_docs"},
		Risk: catalog.RiskPrivileged,
	}
	privateHook := hooks.Definition{
		ID: "child-policy", Reference: "user/private/child-policy", Scope: hooks.ScopeUser,
		Source: "hooks.json", Visibility: hooks.VisibilityAgentPrivate,
		Event: hooks.EventPreToolUse, Command: "true", Timeout: time.Second,
	}
	dispatcher := &customSubagentDispatcher{
		privateMCP: map[string]codingmcp.ConnectedServer{
			"private_docs": {
				ID: "private_docs", Fingerprint: strings.Repeat("a", 64),
				Visibility: codingmcp.VisibilityAgentPrivate, Entries: []catalog.Entry{entry},
			},
		},
		privateHooks: map[string]hooks.Definition{"child-policy": privateHook},
	}
	definition := agentprofile.Definition{
		MCP:   agentprofile.MCPSelection{Private: []string{"private_docs"}},
		Hooks: agentprofile.HookSelection{Private: []string{"child-policy"}},
	}
	mcpBindings, entries, err := dispatcher.compilePrivateMCP(definition.MCP)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "private_search", entries[0].Tool.Decl().Name)
	descriptors := descriptorsForEntries(entries)
	capabilities, err := compileDelegableCapabilities(agentprofile.ToolSelection{}, "", descriptors)
	require.NoError(t, err)
	assert.Empty(t, capabilities, "binding a private server does not select any tool by itself")
	capabilities, err = compileDelegableCapabilities(agentprofile.ToolSelection{Allow: []agentprofile.Selector{{
		Kind: agentprofile.SelectorSource, Value: "mcp/private_docs",
	}}}, "", descriptors)
	require.NoError(t, err)
	assert.Equal(t, []subagent.EffectiveCapability{{
		WireName: "private_search", Source: "mcp/private_docs", Risk: "privileged",
	}}, capabilities)

	hookBindings, definitions, err := dispatcher.compilePrivateHooks(definition.Hooks)
	require.NoError(t, err)
	require.Len(t, definitions, 1)

	selectedEntries, selectedHooks, err := dispatcher.validatePrivateBindings(definition, subagent.ExecutionPlan{
		PrivateMCP: mcpBindings, PrivateHooks: hookBindings,
	})
	require.NoError(t, err)
	require.Len(t, selectedEntries, 1)
	require.Len(t, selectedHooks, 1)

	stale := slices.Clone(mcpBindings)
	stale[0].Fingerprint = strings.Repeat("b", 64)
	_, _, err = dispatcher.validatePrivateBindings(definition, subagent.ExecutionPlan{
		PrivateMCP: stale, PrivateHooks: hookBindings,
	})
	require.ErrorIs(t, err, subagent.ErrInvalid)
	_, _, err = dispatcher.compilePrivateMCP(agentprofile.MCPSelection{Private: []string{"ambient_or_missing"}})
	require.ErrorIs(t, err, subagent.ErrInvalid)
}

func TestCompileDelegableCapabilitiesByMode(t *testing.T) {
	t.Parallel()

	ambient := []catalog.Descriptor{
		{Name: "read", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskRead},
		{Name: "ls", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskRead},
		{Name: "glob", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskRead},
		{Name: "grep", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskRead},
		{Name: "Edit", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskWrite},
		{Name: "Write", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskWrite},
		{Name: "shell", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskPrivileged},
		{Name: "ask_user", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.question"}, Risk: catalog.RiskRead},
		{Name: harness.SkillToolName, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.skills"}, Risk: catalog.RiskRead},
		{Name: tools.HistoryName, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.history"}, Risk: catalog.RiskRead},
		{Name: "mcp_search", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "github"}, Risk: catalog.RiskWrite},
		{Name: "spawn_subagent", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"}, Risk: catalog.RiskRead},
		{Name: SendMessageToolName, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"}, Risk: catalog.RiskRead},
	}

	readOnly := []string{"read", "ls", "glob", "grep", "ask_user", harness.SkillToolName, tools.HistoryName, "mcp_search"}

	tests := []struct {
		name string
		mode agentprofile.CapabilityMode
		want []string
	}{
		{"read-only", agentprofile.CapabilityReadOnly, readOnly},
		{
			"read-write", agentprofile.CapabilityReadWrite,
			[]string{
				"read", "ls", "glob", "grep", "Edit", "Write", "ask_user",
				harness.SkillToolName, tools.HistoryName, "mcp_search", SendMessageToolName,
			},
		},
		{
			"execute", agentprofile.CapabilityExecute,
			[]string{
				"read", "ls", "glob", "grep", "shell", "ask_user",
				harness.SkillToolName, tools.HistoryName, "mcp_search", SendMessageToolName,
			},
		},
		{
			"all", agentprofile.CapabilityAll,
			[]string{
				"read", "ls", "glob", "grep", "Edit", "Write", "shell",
				"ask_user", harness.SkillToolName, tools.HistoryName, "mcp_search",
				SendMessageToolName,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			capabilities, err := compileDelegableCapabilities(agentprofile.ToolSelection{}, test.mode, ambient)
			require.NoError(t, err)

			names := make([]string, len(capabilities))
			for index, capability := range capabilities {
				names[index] = capability.WireName
			}

			assert.Equal(t, test.want, names)
		})
	}

	// An explicit allowlist is already constrained, so a mode does not apply.
	explicit, err := compileDelegableCapabilities(agentprofile.ToolSelection{
		Allow: []agentprofile.Selector{{Kind: agentprofile.SelectorTool, Value: "shell"}},
	}, agentprofile.CapabilityReadOnly, ambient)
	require.NoError(t, err)
	assert.Equal(t, []string{"shell"}, []string{explicit[0].WireName})
}

func TestBuiltinAgentEditStrategies(t *testing.T) {
	t.Parallel()

	byID := make(map[string]agentprofile.Definition)
	for _, definition := range agentprofile.BuiltinDefinitions() {
		byID[definition.ID] = definition
	}

	assert.Empty(t, byID["general-purpose"].EditStrategy,
		"the general-purpose type inherits the parent strategy")

	for _, id := range []string{"explore", "plan", "review"} {
		assert.Equal(t, config.EditStrategyNone, byID[id].EditStrategy, id)
	}
}

func TestResolveChildEditStrategyInheritsOrOverrides(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		config.EditStrategyReplace,
		resolveChildEditStrategy(agentprofile.Definition{}, config.EditStrategyReplace),
	)
	assert.Equal(t,
		config.EditStrategyWriteAndReplace,
		resolveChildEditStrategy(agentprofile.Definition{}, config.EditStrategyWriteAndReplace),
	)
	assert.Equal(t,
		config.EditStrategyNone,
		resolveChildEditStrategy(
			agentprofile.Definition{EditStrategy: config.EditStrategyNone},
			config.EditStrategyWriteAndReplace,
		),
	)
}
