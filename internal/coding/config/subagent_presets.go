package config

import (
	"fmt"
	"strings"
)

// SubagentRole is a named defaults preset for one subagent type. A role narrows
// the type's capability mode and supplies model, reasoning, prompt-file, and
// isolation defaults; it never defines a toolset. It is selected by the agent
// type name and it is not a spawn argument.
type SubagentRole struct {
	// Description is the required human-readable summary of the role.
	Description string `toml:"description"`
	// DefaultCapabilityMode narrows the tool kinds of the type that names it.
	// The value is validated where the role is resolved, because the capability
	// vocabulary belongs to the agent profile layer.
	DefaultCapabilityMode string `toml:"default_capability_mode"`
	// Model is a configured model reference used when the spawn names none.
	Model string `toml:"model"`
	// ReasoningEffort is a portable reasoning level for the child model.
	ReasoningEffort string `toml:"reasoning_effort"`
	// PromptFile is a file rendered into the child system prompt inside a
	// <role-instructions> block. A file-based role resolves it inside its own
	// directory; an inline role resolves it inside the workspace.
	PromptFile string `toml:"prompt_file"`
	// DefaultIsolation is the isolation mode requested for the type.
	DefaultIsolation string `toml:"default_isolation"`

	// Prompt is the resolved PromptFile content. It is empty when no file is
	// configured or the file could not be read.
	Prompt string `toml:"-"`
	// Source is the safe provenance string of the declaration.
	Source string `toml:"-"`
}

// SubagentPersona is a named behavioral overlay. Its instructions are injected
// into the child's initial message list as a synthetic user item; a persona has
// no capability mode. It is selected by the agent type name and it is not a
// spawn argument.
type SubagentPersona struct {
	// Instructions is the inline behavioral text.
	Instructions string `toml:"instructions"`
	// Description is the optional human-readable summary.
	Description string `toml:"description"`
	// InstructionsFile is a file whose content is appended to Instructions.
	InstructionsFile string `toml:"instructions_file"`
	// Inputs and Outputs describe the persona's declared contract. pips parses
	// and validates them for configuration fidelity; nothing surfaces them to
	// the parent model yet.
	Inputs  []SubagentIOField `toml:"inputs"`
	Outputs []SubagentIOField `toml:"outputs"`
	// DefaultIsolation is the isolation mode requested for the type.
	DefaultIsolation string `toml:"default_isolation"`
	// Model is a configured model reference used when nothing above names one.
	Model string `toml:"model"`
	// ReasoningEffort is a portable reasoning level for the child model.
	ReasoningEffort string `toml:"reasoning_effort"`

	// Reminder is the resolved inline-plus-file instruction text.
	Reminder string `toml:"-"`
	// Source is the safe provenance string of the declaration.
	Source string `toml:"-"`
}

// SubagentIOField is one declared persona input or output.
type SubagentIOField struct {
	// Name is the field identifier.
	Name string `toml:"name"`
	// Type is the field's declared shape. It defaults to "file".
	Type string `toml:"type"`
	// Required reports whether the field must be supplied.
	Required bool `toml:"required"`
	// Description explains the field.
	Description string `toml:"description"`
}

// SubagentRoleSourceInline is the provenance string of a role or persona
// declared inside config.toml rather than in its own file.
const SubagentRoleSourceInline = "config"

// ValidateSubagentRole checks the declaration-level shape of one role. It does
// not validate the capability-mode spelling, which the agent profile layer owns.
func ValidateSubagentRole(name string, role SubagentRole) error {
	if strings.TrimSpace(role.Description) == "" {
		return fmt.Errorf("%w: subagent role %q requires a description", ErrInvalid, name)
	}

	return nil
}

// ValidateSubagentPersona checks the declaration-level shape of one persona.
func ValidateSubagentPersona(name string, persona SubagentPersona) error {
	for index, field := range persona.Inputs {
		if err := validateSubagentIOField(name, "input", index, field); err != nil {
			return err
		}
	}

	for index, field := range persona.Outputs {
		if err := validateSubagentIOField(name, "output", index, field); err != nil {
			return err
		}
	}

	return nil
}

func validateSubagentIOField(name, kind string, index int, field SubagentIOField) error {
	if strings.TrimSpace(field.Name) == "" {
		return fmt.Errorf("%w: subagent persona %q %s %d requires a name", ErrInvalid, name, kind, index)
	}

	if strings.TrimSpace(field.Description) == "" {
		return fmt.Errorf("%w: subagent persona %q %s %q requires a description", ErrInvalid, name, kind, field.Name)
	}

	return nil
}
