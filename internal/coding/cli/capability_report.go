package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/rsbin1178/pips/ai"
)

// codingEnforcedCapabilities lists the capability declarations the coding
// agent actually consumes on the chat path. Declarations outside this set are
// accepted and reported, but stay advisory until a consumer reads them.
//
// tools is enforced as a validation rule (declaring false is rejected), and is
// never reported as an unenforced override.
var codingEnforcedCapabilities = map[string]struct{}{
	"vision":            {},
	"structured_output": {},
	"tools":             {},
}

// capabilityNote is one declared capability and whether a consumer reads it.
type capabilityNote struct {
	Name     string
	Value    bool
	Enforced bool
}

// capabilityNotes returns the declared capabilities in declaration order.
func capabilityNotes(override ai.CapabilityOverride) []capabilityNote {
	declarations := override.Declarations()

	notes := make([]capabilityNote, 0, len(declarations))
	for _, declaration := range declarations {
		_, enforced := codingEnforcedCapabilities[declaration.Name]

		notes = append(notes, capabilityNote{
			Name:     declaration.Name,
			Value:    declaration.Value,
			Enforced: enforced,
		})
	}

	return notes
}

// capabilityWarnings returns declaration-level problems worth surfacing.
func capabilityWarnings(override ai.CapabilityOverride) []string {
	var warnings []string

	for _, declaration := range override.Declarations() {
		if declaration.Name == "text" && !declaration.Value {
			warnings = append(warnings, "text = false leaves the coding model unable to produce a reply")
		}
	}

	return warnings
}

// writeCapabilityDeclarations prints one line per declared capability. It is
// shared by `pips config show` so the declaration source stays visible.
func writeCapabilityDeclarations(output io.Writer, override ai.CapabilityOverride) error {
	for _, note := range capabilityNotes(override) {
		kind := "declared"
		if !note.Enforced {
			kind = "declared advisory"
		}

		if _, err := fmt.Fprintf(
			output,
			"resolved.capabilities.%s = %t # %s\n",
			note.Name,
			note.Value,
			kind,
		); err != nil {
			return err
		}
	}

	return nil
}

// capabilityDoctorLine summarizes declarations for `pips doctor`, or returns an
// empty string when nothing is declared.
func capabilityDoctorLine(override ai.CapabilityOverride) string {
	notes := capabilityNotes(override)
	if len(notes) == 0 {
		return ""
	}

	declared := make([]string, 0, len(notes))

	var advisory []string

	for _, note := range notes {
		declared = append(declared, note.Name)
		if !note.Enforced {
			advisory = append(advisory, note.Name)
		}
	}

	var builder strings.Builder
	builder.WriteString("capabilities declared " + strings.Join(declared, " "))

	if len(advisory) > 0 {
		builder.WriteString("\ncapabilities advisory " + strings.Join(advisory, " "))
	}

	for _, warning := range capabilityWarnings(override) {
		builder.WriteString("\ncapabilities warning " + warning)
	}

	return builder.String()
}
