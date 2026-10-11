package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// LegacyWorkspaceFile is the pre-v2 workspace registry that sits beside the
// session store root.
const LegacyWorkspaceFile = "workspaces.json"

// legacyWorkspaceRegistrySchema is the only registry schema this resolver
// understands; any other value yields an empty registry.
const legacyWorkspaceRegistrySchema = "pips.workspaces/v1alpha1"

const maxLegacyWorkspaceRegistryBytes = 4 << 20

// LegacyWorkspaceRegistry resolves a workspace identity to the path it was
// trusted at.
//
// A pre-v2 session header recorded the workspace identity but not always the
// path, so the identity alone is not enough to place a session in its group.
// The workspace registry is what recovers the path, which is why a migration
// must consult it rather than grouping an identified session under "unknown".
type LegacyWorkspaceRegistry struct {
	paths map[string]string
}

// LoadLegacyWorkspaceRegistry reads the registry at path. A missing, oversized,
// unreadable, or unknown-schema file yields an empty registry: the registry only
// improves attribution, so it never fails a migration.
func LoadLegacyWorkspaceRegistry(path string) LegacyWorkspaceRegistry {
	registry := LegacyWorkspaceRegistry{paths: map[string]string{}}

	file, err := os.Open(path) //nolint:gosec // the caller owns the store root
	if err != nil {
		return registry
	}
	defer func() { _ = file.Close() }()

	data, err := readBoundedFile(file, maxLegacyWorkspaceRegistryBytes)
	if err != nil {
		return registry
	}

	var document struct {
		Schema     string `json:"schema"`
		Workspaces map[string]struct {
			Path string `json:"path"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return registry
	}

	if document.Schema != legacyWorkspaceRegistrySchema {
		return registry
	}

	for identity, record := range document.Workspaces {
		trimmed := strings.TrimSpace(record.Path)
		if identity == "" || trimmed == "" {
			continue
		}

		registry.paths[identity] = trimmed
	}

	return registry
}

// DefaultLegacyWorkspaceRegistryPath returns the registry beside a session store
// root, which is where the pre-v2 layout kept it.
func DefaultLegacyWorkspaceRegistryPath(storeRoot string) string {
	trimmed := strings.TrimSpace(storeRoot)
	if trimmed == "" {
		return ""
	}

	return filepath.Join(filepath.Dir(filepath.Clean(trimmed)), LegacyWorkspaceFile)
}

// Path returns the recorded path for one workspace identity.
func (r LegacyWorkspaceRegistry) Path(workspaceID string) (string, bool) {
	path, ok := r.paths[strings.TrimSpace(workspaceID)]

	return path, ok
}

// Len returns how many identities the registry resolved.
func (r LegacyWorkspaceRegistry) Len() int { return len(r.paths) }

// resolveLegacyWorkspace recovers the workspace identity and path recorded in a
// pre-v2 session header.
//
// The recorded path wins. When the header recorded only the identity, the
// registry supplies the path. The identity itself is never discarded: only a
// header that carries none is reported as unknown, so a session that can be
// attributed stays attributable even when its path cannot be recovered.
func resolveLegacyWorkspace(
	extra map[string]string,
	registry LegacyWorkspaceRegistry,
) (workspaceID, workspacePath string) {
	workspaceID = strings.TrimSpace(extra[extraWorkspaceID])
	workspacePath = strings.TrimSpace(extra[extraWorkspacePath])

	if workspacePath == "" && workspaceID != "" {
		if recorded, ok := registry.Path(workspaceID); ok {
			workspacePath = recorded
		}
	}

	if workspaceID == "" {
		workspaceID = legacyUnknownWorkspaceID
	}

	return workspaceID, workspacePath
}
