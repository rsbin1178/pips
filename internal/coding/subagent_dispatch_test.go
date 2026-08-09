package coding

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/internal/coding/agentprofile"
	"github.com/rsbin/pips/internal/coding/subagent"
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
			Name: "run_subagent", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"},
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
	}, ambient)
	require.NoError(t, err)
	assert.Equal(t, []subagent.EffectiveCapability{
		{WireName: "read", Source: "local/coding", Risk: "read"},
		{WireName: "shell", Source: "local/coding", Risk: "privileged"},
	}, capabilities)

	_, err = compileDelegableCapabilities(agentprofile.ToolSelection{
		Allow:   []agentprofile.Selector{{Kind: agentprofile.SelectorTool, Value: "run_subagent"}},
		Require: []agentprofile.Selector{{Kind: agentprofile.SelectorTool, Value: "run_subagent"}},
	}, ambient)
	require.ErrorIs(t, err, subagent.ErrInvalid)
	assert.Contains(t, err.Error(), "required capability")
}

func TestCompileDelegableCapabilitiesProperties(t *testing.T) {
	t.Parallel()

	ambient := []catalog.Descriptor{
		{Name: "read", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskRead, Tags: []string{"filesystem"}},
		{Name: "apply_patch", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskWrite, Tags: []string{"filesystem"}},
		{Name: "shell", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"}, Risk: catalog.RiskPrivileged, Tags: []string{"process"}},
		{Name: "search", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "github"}, Risk: catalog.RiskRead, Tags: []string{"remote"}},
		{Name: "update_issue", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "github"}, Risk: catalog.RiskWrite, Tags: []string{"remote"}},
		{Name: "admin", Source: catalog.Source{Kind: catalog.SourceMCP, ID: "github"}, Risk: catalog.RiskPrivileged, Tags: []string{"remote"}},
		{Name: "ask_user", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.question"}, Risk: catalog.RiskRead, Tags: []string{"input"}},
		{Name: harness.SkillToolName, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.skills"}, Risk: catalog.RiskRead, Tags: []string{"skills"}},
		{Name: "run_subagent", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"}, Risk: catalog.RiskRead, Tags: []string{"delegation"}},
		{Name: "write_plan", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.plan"}, Risk: catalog.RiskWrite, Tags: []string{"planning"}},
	}
	selectors := []agentprofile.Selector{
		{Kind: agentprofile.SelectorTool, Value: "read"},
		{Kind: agentprofile.SelectorTool, Value: "apply_patch"},
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

		capabilities, err := compileDelegableCapabilities(agentprofile.ToolSelection{Allow: allow}, ambient)
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
	baseline, err := compileDelegableCapabilities(selection, ambient)
	require.NoError(t, err)

	injected := append(slices.Clone(ambient),
		catalog.Descriptor{
			Name: "run_subagent", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"},
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

	capabilities, err := compileDelegableCapabilities(selection, injected)
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
			selector: agentprofile.Selector{Kind: agentprofile.SelectorTool, Value: "run_subagent"},
			descriptor: catalog.Descriptor{
				Name: "run_subagent", Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding.subagent"},
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
			_, err := compileDelegableCapabilities(selection, []catalog.Descriptor{test.descriptor})
			require.ErrorIs(t, err, subagent.ErrInvalid)
			assert.Contains(t, err.Error(), fmt.Sprintf("required capability %q", test.selector.String()))
		})
	}
}

func TestCompileProfileLimitsOnlyNarrowsCeiling(t *testing.T) {
	t.Parallel()

	ceiling := subagent.ProductionLimits()

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
