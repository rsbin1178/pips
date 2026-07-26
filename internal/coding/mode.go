package coding

import "github.com/rsbin/pips/internal/coding/config"

// OperatingMode selects the process-local Coding Agent capability policy.
// It is intentionally distinct from Phase, which describes lifecycle state.
type OperatingMode = config.OperatingMode

// Supported Coding Agent operating modes.
const (
	ModeAgent = config.ModeAgent
	ModePlan  = config.ModePlan
)

func validOperatingMode(mode OperatingMode) bool {
	switch mode {
	case ModeAgent, ModePlan:
		return true
	default:
		return false
	}
}
