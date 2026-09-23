package coding

import (
	"context"
	"fmt"
	"slices"

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

// leasedToolGuard denies calls outside the interaction's leased toolset.
// searchName is the deferred discovery tool name when Tool Search is enabled
// and empty when it is disabled.
func leasedToolGuard(
	mode OperatingMode,
	descriptors []catalog.Descriptor,
	searchName string,
	known []agent.Tool,
) func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
	authorized := make(map[string]struct{}, len(descriptors)+1)
	for _, descriptor := range descriptors {
		authorized[descriptor.Name] = struct{}{}
	}

	if searchName != "" {
		// The discovery tool is synthesized after catalog policy has already
		// bounded its result set, and it stays callable even while hidden so
		// a blind call receives the registry hint. Its activation path
		// re-authorizes every selected tool.
		authorized[searchName] = struct{}{}
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

	knownNames := make([]string, 0, len(exists))
	for name := range exists {
		knownNames = append(knownNames, name)
	}

	slices.Sort(knownNames)

	return func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
		if _, ok := authorized[info.Name]; ok {
			return agent.ToolDecision{}
		}

		if _, ok := exists[info.Name]; !ok {
			return agent.DenyTool(unknownToolReason(info.Name, knownNames))
		}

		return agent.DenyTool(fmt.Sprintf(
			"tool %q is unavailable in %s mode",
			info.Name,
			mode,
		))
	}
}
