package coding

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
)

// catalogPolicyForMode leases the model-visible toolset. Plan mode keeps the
// ordinary toolset on purpose: file edits are rejected by the plan edit gate
// rather than hidden from the model.
func catalogPolicyForMode(mode OperatingMode, tenantID string) (catalog.Policy, error) {
	switch mode {
	case ModeAgent, ModePlan:
		return catalog.AllowAll(tenantID, catalog.RiskPrivileged), nil
	default:
		return catalog.Policy{}, fmt.Errorf("%w: unsupported operating mode %q", ErrRuntimeInvalid, mode)
	}
}

func leasedToolGuard(
	mode OperatingMode,
	descriptors []catalog.Descriptor,
	toolSearch bool,
	known []agent.Tool,
) func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
	authorized := make(map[string]struct{}, len(descriptors)+1)
	for _, descriptor := range descriptors {
		authorized[descriptor.Name] = struct{}{}
	}

	if toolSearch {
		// tool_search is synthesized after catalog policy has already bounded
		// its result set. Its activation path re-authorizes every selected tool.
		authorized["tool_search"] = struct{}{}
	}

	// A name the toolbox has never heard of is a model slip, not a policy
	// decision. Reporting it as mode-unavailable misleads the model into
	// believing the capability was disabled instead of retrying the right name.
	exists := make(map[string]struct{}, len(known)+len(authorized))
	for _, tool := range known {
		exists[tool.Decl().Name] = struct{}{}
	}
	for name := range authorized {
		exists[name] = struct{}{}
	}

	return func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
		if _, ok := authorized[info.Name]; ok {
			return agent.ToolDecision{}
		}

		if _, ok := exists[info.Name]; !ok {
			return agent.DenyTool(fmt.Sprintf("unknown tool %q", info.Name))
		}

		return agent.DenyTool(fmt.Sprintf(
			"tool %q is unavailable in %s mode",
			info.Name,
			mode,
		))
	}
}
