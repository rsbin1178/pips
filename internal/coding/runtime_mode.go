package coding

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/tools"
)

func catalogPolicyForMode(mode OperatingMode, tenantID string) (catalog.Policy, error) {
	switch mode {
	case ModeAgent:
		return catalog.AllowAll(tenantID, catalog.RiskPrivileged), nil
	case ModePlan:
		return catalog.Policy{
			TenantID: tenantID, Allowlist: []string{"*"}, MaxRisk: catalog.RiskWrite,
			Authorize: func(_ context.Context, _ string, descriptor catalog.Descriptor) (bool, error) {
				if descriptor.Risk == catalog.RiskRead {
					return true, nil
				}

				return descriptor.Risk == catalog.RiskWrite &&
					(descriptor.Name == tools.WritePlanName || descriptor.Name == planreview.PresentToolName) &&
					descriptor.Source.Kind == catalog.SourceLocal &&
					descriptor.Source.ID == tools.PlanCatalogID, nil
			},
		}, nil
	default:
		return catalog.Policy{}, fmt.Errorf("%w: unsupported operating mode %q", ErrRuntimeInvalid, mode)
	}
}

func leasedToolGuard(
	mode OperatingMode,
	descriptors []catalog.Descriptor,
	toolSearch bool,
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

	return func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
		if _, ok := authorized[info.Name]; ok {
			return agent.ToolDecision{}
		}

		return agent.DenyTool(fmt.Sprintf(
			"tool %q is unavailable in %s mode",
			info.Name,
			mode,
		))
	}
}
