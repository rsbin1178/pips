//nolint:wsl_v5 // Definition merge keeps duplicate and limit checks adjacent.
package mcp

import (
	"fmt"
)

// MergeIsolated appends extra definitions in order. A duplicate ID or an
// extra entry beyond the configured server limit is skipped with one
// diagnostic; the remaining definitions still load.
func MergeIsolated(
	base Definitions,
	extra Definitions,
	maximum int,
) (Definitions, []ConnectionDiagnostic, error) {
	values := base.List()
	seen := make(map[string]struct{}, len(values))
	for _, definition := range values {
		seen[definition.ID] = struct{}{}
	}

	diagnostics := make([]ConnectionDiagnostic, 0)

	for _, definition := range extra.List() {
		if _, duplicate := seen[definition.ID]; duplicate {
			diagnostics = append(diagnostics, connectionDiagnostic(
				definition.ID, "configuration", "definition_duplicate",
				fmt.Sprintf(
					"Agent Plugin MCP server %q conflicts with another configured server and was ignored",
					definition.ID,
				),
			))

			continue
		}
		if len(values) >= maximum {
			diagnostics = append(diagnostics, connectionDiagnostic(
				definition.ID, "configuration", "definition_limit",
				fmt.Sprintf(
					"Agent Plugin MCP server %q exceeds the client-wide server limit and was ignored",
					definition.ID,
				),
			))

			continue
		}
		seen[definition.ID] = struct{}{}
		values = append(values, definition)
	}

	merged, err := NewDefinitions(values, maximum)
	if err != nil {
		return Definitions{}, nil, err
	}

	return merged, diagnostics, nil
}
