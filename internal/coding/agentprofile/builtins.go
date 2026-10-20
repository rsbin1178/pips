package agentprofile

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/rsbin1178/pips/internal/coding/config"
)

// BuiltinDefinitions returns the program-owned compatibility identities. Their
// prompt and optional structured output contract are rendered by the subagent
// package when the unified dispatcher compiles one into an execution plan.
//
// The toolsets mirror the source built-ins: general-purpose keeps the full
// delegable catalog, while explore, plan, and review are read-only. review is a
// pips extension.
func BuiltinDefinitions() []Definition {
	definitions := []Definition{
		builtinDefinition(
			"general-purpose",
			"General purpose",
			"General purpose agent for multi-step tasks.",
			CapabilityAll,
			"",
		),
		builtinDefinition(
			"explore",
			"Explore",
			"Fast, read-only agent specialized for codebase exploration.",
			CapabilityReadOnly,
			config.EditStrategyNone,
		),
		builtinDefinition(
			"plan",
			"Plan",
			"Software architect for planning implementation strategies.",
			CapabilityReadOnly,
			config.EditStrategyNone,
		),
		builtinDefinition(
			"review",
			"Review",
			"Report evidence-backed defects and risks.",
			CapabilityReadOnly,
			config.EditStrategyNone,
		),
	}

	return definitions
}

func builtinDefinition(
	id, name, description string,
	mode CapabilityMode,
	editStrategy config.EditStrategy,
) Definition {
	digest := sha256.Sum256([]byte("pips builtin agent/v1\n" + id))

	return Definition{
		ID:             id,
		Kind:           KindBuiltin,
		Scope:          ScopeBuiltin,
		Source:         "builtin:" + id,
		Digest:         hex.EncodeToString(digest[:]),
		Schema:         "pips.agent/builtin/v1",
		Name:           name,
		Description:    description,
		Model:          modelInherit,
		CapabilityMode: mode,
		EditStrategy:   editStrategy,
		Visibility:     Visibility{User: true, Model: true},
		Delivery:       []Delivery{DeliveryForeground, DeliveryBackground},
		Output:         OutputContract{Format: OutputText},
	}
}
