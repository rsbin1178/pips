//nolint:wsl_v5 // Typed manifest evidence is assembled and cross-validated in one linear pass.
package teamintegration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// BlobReader returns exact raw Git blob bytes without filters.
type BlobReader interface {
	Blob(context.Context, string) ([]byte, error)
}

// BuildManifest creates a canonical Team-base to composed-tree manifest.
//
//nolint:gocyclo // Manifest construction accounts for every exact typed transition and budget.
func BuildManifest(
	ctx context.Context,
	reader BlobReader,
	baseEntries []Entry,
	targetEntries []Entry,
	limits Limits,
) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if reader == nil {
		return Manifest{}, fmt.Errorf("%w: nil blob reader", ErrInvalid)
	}
	if err := validateLimits(limits); err != nil {
		return Manifest{}, err
	}

	base, err := entryMap(baseEntries, limits)
	if err != nil {
		return Manifest{}, err
	}
	target, err := entryMap(targetEntries, limits)
	if err != nil {
		return Manifest{}, err
	}

	paths := changedPaths(base, target)
	entries := make([]ManifestEntry, 0, len(paths))
	var total int64
	manifest := Manifest{Entries: entries}

	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}

		baseEntry, hasBase := base[path]
		targetEntry, hasTarget := target[path]

		baseState, size, err := readFileState(ctx, reader, baseEntry, hasBase, limits)
		if err != nil {
			return Manifest{}, fmt.Errorf("manifest base %q: %w", path, err)
		}
		total, err = addTreeBytes(total, size, limits.TreeBytes)
		if err != nil {
			return Manifest{}, err
		}

		targetState, size, err := readFileState(ctx, reader, targetEntry, hasTarget, limits)
		if err != nil {
			return Manifest{}, fmt.Errorf("manifest target %q: %w", path, err)
		}
		total, err = addTreeBytes(total, size, limits.TreeBytes)
		if err != nil {
			return Manifest{}, err
		}

		operation := OperationReplace
		switch {
		case !hasBase:
			operation = OperationAdd
			manifest.Added++
		case !hasTarget:
			operation = OperationDelete
			manifest.Deleted++
		default:
			manifest.Changed++
		}
		if baseState.Binary || targetState.Binary {
			manifest.Binary++
		}

		manifest.Entries = append(manifest.Entries, ManifestEntry{
			Path: path, Operation: operation, Base: baseState, Target: targetState,
		})
	}

	manifest.Digest, err = digestValue(struct {
		Entries []ManifestEntry `json:"entries"`
	}{Entries: manifest.Entries})
	if err != nil {
		return Manifest{}, err
	}

	return manifest, nil
}

func readFileState(
	ctx context.Context,
	reader BlobReader,
	entry Entry,
	exists bool,
	limits Limits,
) (FileState, int64, error) {
	if !exists {
		return FileState{Kind: FileAbsent}, 0, nil
	}

	content, err := reader.Blob(ctx, entry.OID)
	if err != nil {
		return FileState{}, 0, err
	}
	size := int64(len(content))
	if size > limits.BlobBytes {
		return FileState{}, 0, ErrLimit
	}
	if entry.Mode == "120000" && bytes.IndexByte(content, 0) >= 0 {
		return FileState{}, 0, fmt.Errorf("%w: symlink target contains nul", ErrInvalid)
	}

	kind := FileRegular
	if entry.Mode == "120000" {
		kind = FileSymlink
	}
	sum := sha256.Sum256(content)

	return FileState{
		Kind: kind, Mode: entry.Mode, OID: entry.OID, Size: size,
		SHA256: hex.EncodeToString(sum[:]), Binary: bytes.IndexByte(content, 0) >= 0,
	}, size, nil
}

func addTreeBytes(current, value, maximum int64) (int64, error) {
	if value < 0 || current > maximum-value {
		return 0, ErrLimit
	}

	return current + value, nil
}

// SortedManifestPaths returns an owned, stable path list for preview callers.
func SortedManifestPaths(manifest Manifest) []string {
	paths := make([]string, 0, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		paths = append(paths, entry.Path)
	}
	sort.Strings(paths)

	return paths
}

//nolint:gocyclo // Persisted evidence counters and operations are cross-validated in one pass.
func validateManifestEvidence(manifest Manifest, limits Limits) error {
	if err := validateLimits(limits); err != nil {
		return err
	}
	if len(manifest.Entries) > limits.Entries || manifest.Digest == "" {
		return ErrLimit
	}
	added, changed, deleted, binary := 0, 0, 0, 0
	previous := ""
	for _, entry := range manifest.Entries {
		path, err := workspace.NormalizePath(entry.Path, false)
		if err != nil || path != entry.Path || len(path) > limits.PathBytes ||
			previous != "" && previous >= path {
			return fmt.Errorf("%w: invalid manifest path", ErrInvalid)
		}
		previous = path
		if err := validateFileState(entry.Base, limits); err != nil {
			return err
		}
		if err := validateFileState(entry.Target, limits); err != nil {
			return err
		}
		switch entry.Operation {
		case OperationAdd:
			if entry.Base.Kind != FileAbsent || entry.Target.Kind == FileAbsent {
				return fmt.Errorf("%w: invalid add", ErrInvalid)
			}
			added++
		case OperationReplace:
			if entry.Base.Kind == FileAbsent || entry.Target.Kind == FileAbsent {
				return fmt.Errorf("%w: invalid replace", ErrInvalid)
			}
			changed++
		case OperationDelete:
			if entry.Base.Kind == FileAbsent || entry.Target.Kind != FileAbsent {
				return fmt.Errorf("%w: invalid delete", ErrInvalid)
			}
			deleted++
		default:
			return fmt.Errorf("%w: invalid manifest operation", ErrInvalid)
		}
		if entry.Base.Binary || entry.Target.Binary {
			binary++
		}
	}
	if manifest.Added != added || manifest.Changed != changed ||
		manifest.Deleted != deleted || manifest.Binary != binary {
		return fmt.Errorf("%w: manifest statistics", ErrInvalid)
	}
	digest, err := digestValue(struct {
		Entries []ManifestEntry `json:"entries"`
	}{Entries: manifest.Entries})
	if err != nil || digest != manifest.Digest {
		return fmt.Errorf("%w: manifest digest", ErrInvalid)
	}

	return nil
}

//nolint:gocyclo // Each supported filesystem kind has an intentionally separate exact shape.
func validateFileState(value FileState, limits Limits) error {
	if value.Kind == FileAbsent {
		if value != (FileState{Kind: FileAbsent}) {
			return fmt.Errorf("%w: absent file metadata", ErrInvalid)
		}

		return nil
	}
	if value.Kind != FileRegular && value.Kind != FileSymlink || value.OID == "" ||
		value.Size < 0 || value.Size > limits.BlobBytes || len(value.SHA256) != sha256.Size*2 {
		return fmt.Errorf("%w: file state", ErrInvalid)
	}
	if _, err := hex.DecodeString(value.SHA256); err != nil {
		return fmt.Errorf("%w: file digest", ErrInvalid)
	}
	if strings.ContainsRune(value.OID, 0) {
		return fmt.Errorf("%w: file object id", ErrInvalid)
	}
	if value.Kind == FileSymlink && value.Mode != "120000" ||
		value.Kind == FileRegular && value.Mode != "100644" && value.Mode != "100755" {
		return fmt.Errorf("%w: file mode", ErrInvalid)
	}

	return nil
}
