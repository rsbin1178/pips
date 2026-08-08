package agentprofile

import (
	"crypto/sha256"
	"encoding/hex"
)

// BuiltinDefinitions returns the program-owned compatibility identities. Their
// exact prompt and typed output adapters remain in subagent until the durable
// protocol migration consumes the common identity model.
func BuiltinDefinitions() []Definition {
	definitions := []Definition{
		builtinDefinition("explore", "Explore", "Gather bounded workspace evidence."),
		builtinDefinition("plan", "Plan", "Produce an evidence-backed implementation plan."),
		builtinDefinition("review", "Review", "Report evidence-backed defects and risks."),
	}

	return definitions
}

func builtinDefinition(id, name, description string) Definition {
	digest := sha256.Sum256([]byte("pips builtin agent/v1\n" + id))

	return Definition{
		ID:          id,
		Kind:        KindBuiltin,
		Scope:       ScopeBuiltin,
		Source:      "builtin:" + id,
		Digest:      hex.EncodeToString(digest[:]),
		Schema:      "pips.agent/builtin/v1",
		Name:        name,
		Description: description,
		Model:       modelInherit,
		Visibility:  Visibility{User: true, Model: true},
		Delivery:    []Delivery{DeliveryForeground, DeliveryBackground},
		Output:      OutputContract{Format: OutputText},
	}
}
