package subagentprofile

import (
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/agentprofile"
	"github.com/rsbin1178/pips/internal/coding/config"
)

// isolation values a role or persona may request. Both are honoured: "none"
// keeps the child in the parent workspace and "worktree" gives it a preserved
// child-owned Worktree.
const (
	isolationNone     = "none"
	isolationWorktree = "worktree"
)

// defaultIOType is the declared shape of an input or output that names none.
const defaultIOType = "file"

// normalizePersona applies the declaration defaults a preset file may omit.
func normalizePersona(persona config.SubagentPersona) config.SubagentPersona {
	for index := range persona.Inputs {
		if persona.Inputs[index].Type == "" {
			persona.Inputs[index].Type = defaultIOType
		}
	}

	for index := range persona.Outputs {
		if persona.Outputs[index].Type == "" {
			persona.Outputs[index].Type = defaultIOType
		}
	}

	return persona
}

func validateRole(name string, role config.SubagentRole, limits Limits) error {
	if err := config.ValidateSubagentRole(name, role); err != nil {
		return err
	}

	if role.DefaultCapabilityMode != "" {
		if _, ok := agentprofile.ParseCapabilityMode(role.DefaultCapabilityMode); !ok {
			return fmt.Errorf(
				"%w: role %q default_capability_mode must be read-only, read-write, execute, or all",
				ErrInvalid, name,
			)
		}
	}

	if err := validateReasoningEffort(name, "role", role.ReasoningEffort); err != nil {
		return err
	}

	if err := validateIsolation(name, "role", role.DefaultIsolation); err != nil {
		return err
	}

	if role.PromptFile != "" && !validRelativeFile(role.PromptFile) {
		return fmt.Errorf("%w: role %q prompt_file must be a clean relative path", ErrInvalid, name)
	}

	if len(role.Prompt) > int(limits.MaxPromptBytes) {
		return fmt.Errorf("%w: role %q prompt exceeds the byte limit", ErrLimitExceeded, name)
	}

	return nil
}

func validatePersona(name string, persona config.SubagentPersona) error {
	if err := config.ValidateSubagentPersona(name, persona); err != nil {
		return err
	}

	if err := validateReasoningEffort(name, "persona", persona.ReasoningEffort); err != nil {
		return err
	}

	if err := validateIsolation(name, "persona", persona.DefaultIsolation); err != nil {
		return err
	}

	return nil
}

// validateReasoningEffort rejects an unknown effort instead of silently
// ignoring a declared setting.
func validateReasoningEffort(name, kind, effort string) error {
	if effort == "" {
		return nil
	}

	switch ai.ReasoningEffort(effort) {
	case ai.ReasoningNone, ai.ReasoningMinimal, ai.ReasoningLow, ai.ReasoningMedium,
		ai.ReasoningHigh, ai.ReasoningXHigh, ai.ReasoningMax:
		return nil
	default:
		return fmt.Errorf("%w: %s %q reasoning_effort %q is not a supported level", ErrInvalid, kind, name, effort)
	}
}

func validateIsolation(name, kind, isolation string) error {
	if isolation == "" {
		return nil
	}

	if isolation != isolationNone && isolation != isolationWorktree {
		return fmt.Errorf("%w: %s %q default_isolation must be none or worktree", ErrInvalid, kind, name)
	}

	return nil
}

// IsolationWorktree reports whether the isolation spelling requests a worktree.
func IsolationWorktree(value string) bool { return strings.TrimSpace(value) == isolationWorktree }
