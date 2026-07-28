//nolint:wsl_v5 // Entry comparison and conflict evidence remain adjacent in stable traversal order.
package teamintegration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rsbin/pips/internal/coding/workspace"
)

// Compose applies each dependency-ordered Attempt delta exactly once.
//
//nolint:gocyclo // Exact three-way entry composition keeps every conflict family auditable.
func Compose(ctx context.Context, selection Selection, limits Limits) (Composition, error) {
	if err := ctx.Err(); err != nil {
		return Composition{}, err
	}

	if err := validateLimits(limits); err != nil {
		return Composition{}, err
	}

	if selection.TeamID == "" || selection.ResourceRevision == 0 || selection.BaseOID == "" ||
		len(selection.Artifacts) == 0 {
		return Composition{}, fmt.Errorf("%w: incomplete selection", ErrInvalid)
	}

	current, err := entryMap(selection.Base, limits)
	if err != nil {
		return Composition{}, fmt.Errorf("%w: Team base: %w", ErrInvalid, err)
	}

	seenTasks := make(map[string]struct{}, len(selection.Artifacts))
	conflicts := make([]Conflict, 0)
	duplicates := 0

	for _, artifact := range selection.Artifacts {
		if err := ctx.Err(); err != nil {
			return Composition{}, err
		}

		if artifact.TaskID == "" || artifact.AttemptID == "" || artifact.BaseOID == "" ||
			artifact.ResultOID == "" {
			return Composition{}, fmt.Errorf("%w: incomplete artifact", ErrInvalid)
		}
		if _, duplicate := seenTasks[artifact.TaskID]; duplicate {
			return Composition{}, fmt.Errorf("%w: duplicate Task %q", ErrInvalid, artifact.TaskID)
		}
		seenTasks[artifact.TaskID] = struct{}{}

		base, err := entryMap(artifact.Base, limits)
		if err != nil {
			return Composition{}, fmt.Errorf("%w: artifact base: %w", ErrInvalid, err)
		}
		target, err := entryMap(artifact.Result, limits)
		if err != nil {
			return Composition{}, fmt.Errorf("%w: artifact result: %w", ErrInvalid, err)
		}

		paths := changedPaths(base, target)
		for _, path := range paths {
			baseEntry, hasBase := base[path]
			targetEntry, hasTarget := target[path]
			currentEntry, hasCurrent := current[path]

			switch {
			case equalOptionalEntry(currentEntry, hasCurrent, baseEntry, hasBase):
				if hasTarget {
					if collision := collidingPath(current, path); collision != "" {
						conflicts = append(conflicts, Conflict{
							TaskID: artifact.TaskID, AttemptID: artifact.AttemptID,
							Path: path, Kind: ConflictPath,
						})
						continue
					}
					current[path] = targetEntry
				} else {
					delete(current, path)
				}
			case equalOptionalEntry(currentEntry, hasCurrent, targetEntry, hasTarget):
				duplicates++
			default:
				conflicts = append(conflicts, Conflict{
					TaskID: artifact.TaskID, AttemptID: artifact.AttemptID,
					Path: path,
					Kind: classifyConflict(
						currentEntry, hasCurrent, baseEntry, hasBase, targetEntry, hasTarget,
					),
				})
			}
		}
	}

	entries := sortedEntries(current)
	if len(entries) > limits.Entries || len(conflicts) > limits.Entries {
		return Composition{}, ErrLimit
	}
	if len(conflicts) != 0 {
		digest, digestErr := digestValue(struct {
			BaseOID   string     `json:"base_oid"`
			Artifacts []string   `json:"artifacts"`
			Entries   []Entry    `json:"entries"`
			Conflicts []Conflict `json:"conflicts"`
		}{
			BaseOID: selection.BaseOID, Artifacts: artifactIdentities(selection.Artifacts),
			Entries: entries, Conflicts: conflicts,
		})
		if digestErr != nil {
			return Composition{}, digestErr
		}

		return Composition{
			Entries: entries, Conflicts: conflicts, Duplicates: duplicates, Digest: digest,
		}, ErrConflict
	}

	digest, err := digestValue(struct {
		BaseOID   string   `json:"base_oid"`
		Artifacts []string `json:"artifacts"`
		Entries   []Entry  `json:"entries"`
	}{
		BaseOID:   selection.BaseOID,
		Artifacts: artifactIdentities(selection.Artifacts),
		Entries:   entries,
	})
	if err != nil {
		return Composition{}, err
	}

	return Composition{
		Entries: entries, Conflicts: conflicts, Duplicates: duplicates, Digest: digest,
	}, nil
}

func entryMap(entries []Entry, limits Limits) (map[string]Entry, error) {
	if len(entries) > limits.Entries {
		return nil, ErrLimit
	}

	result := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		if err := validateEntry(entry, limits); err != nil {
			return nil, err
		}
		if _, duplicate := result[entry.Path]; duplicate {
			return nil, fmt.Errorf("%w: duplicate path %q", ErrInvalid, entry.Path)
		}
		result[entry.Path] = entry
	}

	if path, collision := firstPathCollision(result); collision {
		return nil, fmt.Errorf("%w: file-directory collision at %q", ErrInvalid, path)
	}

	return result, nil
}

func validateEntry(entry Entry, limits Limits) error {
	if entry.Mode != "100644" && entry.Mode != "100755" && entry.Mode != "120000" {
		return fmt.Errorf("%w: unsupported mode %q", ErrInvalid, entry.Mode)
	}
	if entry.OID == "" || strings.ContainsRune(entry.OID, '\x00') {
		return fmt.Errorf("%w: empty object id", ErrInvalid)
	}
	path, err := workspace.NormalizePath(entry.Path, false)
	if err != nil || path != entry.Path || len(entry.Path) > limits.PathBytes {
		return fmt.Errorf("%w: unsafe path %q", ErrInvalid, entry.Path)
	}

	return nil
}

func changedPaths(base, target map[string]Entry) []string {
	paths := make([]string, 0, len(base)+len(target))
	seen := make(map[string]struct{}, len(base)+len(target))
	for path, entry := range base {
		if targetEntry, exists := target[path]; !exists || entry != targetEntry {
			paths = append(paths, path)
			seen[path] = struct{}{}
		}
	}
	for path, entry := range target {
		if _, exists := seen[path]; exists {
			continue
		}
		if baseEntry, exists := base[path]; !exists || entry != baseEntry {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	return paths
}

func equalOptionalEntry(left Entry, hasLeft bool, right Entry, hasRight bool) bool {
	return hasLeft == hasRight && (!hasLeft || left == right)
}

func collidingPath(entries map[string]Entry, path string) string {
	for candidate := range entries {
		if candidate == path {
			continue
		}
		if strings.HasPrefix(candidate, path+"/") || strings.HasPrefix(path, candidate+"/") {
			return candidate
		}
	}

	return ""
}

func firstPathCollision(entries map[string]Entry) (string, bool) {
	for path := range entries {
		for separator := strings.IndexByte(path, '/'); separator >= 0; {
			if _, exists := entries[path[:separator]]; exists {
				return path, true
			}

			next := strings.IndexByte(path[separator+1:], '/')
			if next < 0 {
				break
			}
			separator += next + 1
		}
	}

	return "", false
}

//nolint:gocyclo // Conflict classification is a closed table over file states and modes.
func classifyConflict(
	current Entry,
	hasCurrent bool,
	base Entry,
	hasBase bool,
	target Entry,
	hasTarget bool,
) ConflictKind {
	switch {
	case !hasBase && hasTarget && hasCurrent:
		return ConflictAddAdd
	case hasBase && (!hasTarget || !hasCurrent && hasTarget):
		return ConflictDeleteModify
	case hasCurrent && (current.Mode == "120000" || base.Mode == "120000" || target.Mode == "120000"):
		return ConflictSymlink
	case hasCurrent && hasBase && hasTarget &&
		(current.Mode != base.Mode || current.Mode != target.Mode || base.Mode != target.Mode):
		return ConflictMode
	case hasCurrent:
		return ConflictModifyModify
	default:
		return ConflictUnknown
	}
}

func sortedEntries(entries map[string]Entry) []Entry {
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	slices.SortFunc(result, func(left, right Entry) int {
		return strings.Compare(left.Path, right.Path)
	})

	return result
}

func artifactIdentities(artifacts []Artifact) []string {
	result := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		result = append(result, strings.Join([]string{
			artifact.TaskID, artifact.AttemptID, artifact.BaseOID, artifact.ResultOID,
			artifact.ResultRef,
		}, "\x00"))
	}

	return result
}

func digestValue(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: encode digest: %w", ErrInvalid, err)
	}
	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:]), nil
}

func validateLimits(limits Limits) error {
	if limits.Entries <= 0 || limits.Entries > maximumEntries ||
		limits.PathBytes <= 0 || limits.PathBytes > maximumPathBytes ||
		limits.BlobBytes <= 0 || limits.BlobBytes > maximumBlobBytes ||
		limits.TreeBytes <= 0 || limits.TreeBytes > maximumTreeBytes ||
		limits.BlobBytes > limits.TreeBytes {
		return fmt.Errorf("%w: unsupported limits", ErrInvalid)
	}

	return nil
}
