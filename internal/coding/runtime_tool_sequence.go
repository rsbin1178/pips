package coding

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
)

const mustSequenceAfterResult = "must_sequence_after_result"

type statefulToolBatchGuard struct {
	risks map[string]catalog.Risk
}

func newStatefulToolBatchGuard(descriptors []catalog.Descriptor) statefulToolBatchGuard {
	risks := make(map[string]catalog.Risk, len(descriptors))
	for _, descriptor := range descriptors {
		risks[descriptor.Name] = descriptor.Risk
	}

	return statefulToolBatchGuard{risks: risks}
}

func (g statefulToolBatchGuard) beforeTool(
	_ context.Context,
	info agent.ToolCallInfo,
) agent.ToolDecision {
	risk, known := g.risks[info.Name]
	if !known || risk < catalog.RiskWrite || info.BatchSize <= 1 {
		return agent.ToolDecision{}
	}

	return agent.DenyTool(fmt.Sprintf(
		"%s: stateful tool %q must be called alone after observing prior tool results",
		mustSequenceAfterResult,
		info.Name,
	))
}
