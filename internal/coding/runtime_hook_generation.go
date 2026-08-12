package coding

import (
	"context"
	"fmt"
	"slices"

	"github.com/rsbin1178/pips/internal/coding/hooks"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

func loadRuntimeHookDefinitions(
	ctx context.Context,
	layout paths.Layout,
	tree *workspace.Tree,
	projectTrusted bool,
	workspaceID string,
) ([]hooks.Definition, []hooks.Diagnostic, error) {
	loaded, err := hooks.Load(ctx, hooks.LoadOptions{
		Paths: layout, Tree: tree, ProjectTrusted: projectTrusted, Limits: hooks.DefaultLimits(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("coding runtime: load lifecycle hooks: %w", err)
	}

	statuses, err := hooks.NewTrustStore(layout.HookTrustFile()).Resolve(ctx, loaded, workspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("coding runtime: resolve lifecycle hook trust: %w", err)
	}

	trusted := make([]hooks.Definition, 0, len(statuses))
	diagnostics := make([]hooks.Diagnostic, 0)

	for _, status := range statuses {
		if status.Status == hooks.StatusTrusted {
			trusted = append(trusted, status.Definition)
			continue
		}

		diagnostics = append(diagnostics, hooks.Diagnostic{
			Reference: status.Definition.Reference,
			Code:      "pending_trust",
			Message:   "hook is pending explicit trust; review it with pips hooks list",
		})
	}

	return trusted, diagnostics, nil
}

func ambientHookDefinitions(values []hooks.Definition) []hooks.Definition {
	selected := make([]hooks.Definition, 0, len(values))
	for _, definition := range values {
		if definition.EffectiveVisibility() == hooks.VisibilityAmbient {
			selected = append(selected, definition)
		}
	}

	return slices.Clone(selected)
}

func privateHookDefinitions(values []hooks.Definition) map[string]hooks.Definition {
	selected := make(map[string]hooks.Definition)

	for _, definition := range values {
		if definition.EffectiveVisibility() == hooks.VisibilityAgentPrivate {
			selected[definition.ID] = definition
		}
	}

	return selected
}
