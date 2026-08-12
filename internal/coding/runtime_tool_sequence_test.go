package coding

import (
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/stretchr/testify/assert"
)

func TestStatefulToolBatchGuard(t *testing.T) {
	t.Parallel()

	guard := newStatefulToolBatchGuard([]catalog.Descriptor{
		{Name: "read", Risk: catalog.RiskRead},
		{Name: "patch", Risk: catalog.RiskWrite},
		{Name: "shell", Risk: catalog.RiskPrivileged},
	})

	for _, test := range []struct {
		name      string
		tool      string
		batchSize int
		want      agent.ToolDecisionAction
	}{
		{name: "read in batch", tool: "read", batchSize: 3, want: agent.ToolDecisionAllow},
		{name: "standalone write", tool: "patch", batchSize: 1, want: agent.ToolDecisionAllow},
		{name: "standalone privileged", tool: "shell", batchSize: 1, want: agent.ToolDecisionAllow},
		{name: "write in batch", tool: "patch", batchSize: 2, want: agent.ToolDecisionDeny},
		{name: "privileged in batch", tool: "shell", batchSize: 2, want: agent.ToolDecisionDeny},
		{name: "unknown", tool: "dynamic", batchSize: 2, want: agent.ToolDecisionAllow},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			decision := guard.beforeTool(t.Context(), agent.ToolCallInfo{
				ToolCall:  agent.ToolCall{Name: test.tool},
				BatchSize: test.batchSize,
			})
			assert.Equal(t, test.want, decision.Action)
			if test.want == agent.ToolDecisionDeny {
				assert.Contains(t, decision.Reason, mustSequenceAfterResult)
			}
		})
	}
}
