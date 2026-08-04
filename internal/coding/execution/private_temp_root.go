//nolint:wsl_v5 // Scratch ownership, identity checks, and cleanup form one boundary.
package execution

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// PrivateTempRoot is an owner-only, identity-checked runtime scratch root.
// It is intended for process-local temporary files and cache directories, not
// durable product state. Close refuses to remove a path whose filesystem
// identity changed after allocation.
type PrivateTempRoot struct {
	object    fileObject
	closeOnce sync.Once
	closeErr  error
}

// NewPrivateTempRoot allocates a canonical owner-only scratch directory below
// base. The caller owns the returned root and must close it after all plans and
// child processes have stopped.
func NewPrivateTempRoot(base string) (*PrivateTempRoot, error) {
	if !filepath.IsAbs(base) {
		return nil, fmt.Errorf("%w: private temp base must be absolute", ErrInvalidOperation)
	}

	canonicalBase, err := filepath.EvalSymlinks(filepath.Clean(base))
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize private temp base: %w", ErrInvalidOperation, err)
	}
	baseInfo, err := os.Lstat(canonicalBase)
	if err != nil || !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: private temp base must be a directory", ErrInvalidOperation)
	}

	path, err := os.MkdirTemp(canonicalBase, "pips-")
	if err != nil {
		return nil, fmt.Errorf("create private runtime scratch root: %w", err)
	}

	cleanup := func(cause error) (*PrivateTempRoot, error) {
		return nil, errors.Join(cause, os.Remove(path))
	}

	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // Runtime scratch is deliberately owner-only.
		return cleanup(fmt.Errorf("set private runtime scratch mode: %w", err))
	}

	object, info, err := inspectFileObject(path)
	if err != nil {
		return cleanup(fmt.Errorf("inspect private runtime scratch root: %w", err))
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return cleanup(fmt.Errorf("%w: private runtime scratch root is not owner-only", ErrInvalidOperation))
	}

	return &PrivateTempRoot{object: object}, nil
}

// NewPrivateTempRootOutside allocates an owner-only scratch root below base
// and refuses roots nested under any excluded path. Excluded paths may be
// absent because callers commonly protect a product root before its first
// durable file is created.
func NewPrivateTempRootOutside(base string, excluded []string) (*PrivateTempRoot, error) {
	root, err := NewPrivateTempRoot(base)
	if err != nil {
		return nil, err
	}

	for _, excludedPath := range excluded {
		if excludedPath == "" {
			continue
		}
		if pathContains(canonicalComparisonPath(excludedPath), root.Path()) {
			cleanupErr := root.Close()

			return nil, errors.Join(
				fmt.Errorf("%w: private temp root overlaps excluded path", ErrInvalidOperation),
				cleanupErr,
			)
		}
	}

	return root, nil
}

func canonicalComparisonPath(value string) string {
	absolute, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return filepath.Clean(value)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved)
	}

	return filepath.Clean(absolute)
}

// Path returns the canonical scratch root path.
func (r *PrivateTempRoot) Path() string {
	if r == nil {
		return ""
	}

	return r.object.path
}

// Close removes the scratch root if its identity is unchanged. It is
// idempotent and never follows a replacement symlink.
func (r *PrivateTempRoot) Close() error {
	if r == nil {
		return nil
	}

	r.closeOnce.Do(func() {
		if err := removeOwnedDirectory(r.object); err != nil {
			r.closeErr = fmt.Errorf("coding execution: refuse private runtime scratch cleanup: %w", err)
		}
	})

	return r.closeErr
}

func removeOwnedDirectory(expected fileObject) error {
	return removeOwnedChild(fileObject{}, expected)
}

//nolint:gocyclo,wsl_v5 // Identity checks, quarantine, and cleanup are one fail-closed boundary.
func removeOwnedChild(parentExpected, childExpected fileObject) error {
	if childExpected.path == "" {
		return errors.New("private cleanup target is empty")
	}

	parentPath := filepath.Dir(childExpected.path)
	if parentExpected.path != "" {
		if parentPath != parentExpected.path {
			return errors.New("private cleanup parent does not match expected root")
		}
		if err := revalidatePrivateDirectory(parentExpected); err != nil {
			return fmt.Errorf("private cleanup parent changed: %w", err)
		}
	}

	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return fmt.Errorf("open private cleanup parent: %w", err)
	}
	defer func() { _ = parent.Close() }()

	if parentExpected.path != "" {
		info, statErr := parent.Stat(".")
		if statErr != nil {
			return fmt.Errorf("inspect private cleanup parent: %w", statErr)
		}
		if !fileInfoMatches(info, parentExpected) {
			return errors.New("private cleanup parent identity changed")
		}
	}

	name := filepath.Base(childExpected.path)
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect private cleanup target: %w", err)
	}
	if !fileInfoMatches(info, childExpected) || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.Join(ErrInvalidOperation, errors.New("private cleanup target identity or mode changed"))
	}

	tombstone, err := privateCleanupName(parent)
	if err != nil {
		return err
	}
	if err := parent.Rename(name, tombstone); err != nil {
		return fmt.Errorf("quarantine private cleanup target: %w", err)
	}

	quarantined, err := parent.Lstat(tombstone)
	if err != nil {
		return fmt.Errorf("inspect quarantined private cleanup target: %w", err)
	}
	if !fileInfoMatches(quarantined, childExpected) || !quarantined.IsDir() {
		return errors.Join(ErrInvalidOperation, errors.New("private cleanup target changed during quarantine"))
	}

	if err := parent.RemoveAll(tombstone); err != nil {
		return fmt.Errorf("remove quarantined private cleanup target: %w", err)
	}

	return nil
}

func privateCleanupName(parent *os.Root) (string, error) {
	for range 8 {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate private cleanup name: %w", err)
		}
		name := ".pips-cleanup-" + hex.EncodeToString(random[:])
		if _, err := parent.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", fmt.Errorf("inspect private cleanup name: %w", err)
		}
	}

	return "", errors.New("private cleanup name allocation exhausted")
}

func fileInfoMatches(info os.FileInfo, expected fileObject) bool {
	device, inode, err := fileIdentity(info)
	return err == nil && device == expected.device && inode == expected.inode
}
