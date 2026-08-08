package coding

import (
	"testing"

	"github.com/rsbin/pips/agent/catalog"
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
