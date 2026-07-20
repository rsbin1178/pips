package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// Tree owns confined filesystem access for a Workspace. It is safe for
// concurrent reads; mutations submitted through Mutate are serialized.
type Tree struct {
	root *os.Root
	path string

	lifecycle sync.RWMutex
	closed    bool
	mutation  sync.Mutex
}

// OpenTree opens confined filesystem access for workspace and verifies that
// its canonical directory identity has not changed since Workspace was opened.
func OpenTree(workspace Workspace) (*Tree, error) {
	if !workspace.identity.valid() || workspace.root == "" {
		return nil, fmt.Errorf("%w: empty identity", ErrInvalid)
	}

	root, err := os.OpenRoot(workspace.root)
	if err != nil {
		return nil, fmt.Errorf("coding workspace: open tree: %w", err)
	}

	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("coding workspace: inspect tree: %w", err)
	}

	device, inode, err := fileIdentity(info)
	if err != nil {
		_ = root.Close()
		return nil, err
	}

	if device != workspace.identity.device || inode != workspace.identity.inode {
		_ = root.Close()
		return nil, fmt.Errorf("%w: root replaced before tree open", ErrChanged)
	}

	return &Tree{root: root, path: workspace.root}, nil
}

// Path returns the canonical workspace path for display and process-level
// adapters. File operations must use Tree methods instead.
func (t *Tree) Path() string {
	if t == nil {
		return ""
	}

	return t.path
}

// Close releases the root handle. Repeated calls are safe.
func (t *Tree) Close() error {
	if t == nil {
		return nil
	}

	t.lifecycle.Lock()
	defer t.lifecycle.Unlock()

	if t.closed {
		return nil
	}

	t.closed = true
	if err := t.root.Close(); err != nil {
		return fmt.Errorf("coding workspace: close tree: %w", err)
	}

	return nil
}

// NormalizePath validates and normalizes a slash-separated, relative path.
// The root path "." is accepted only when allowRoot is true.
func NormalizePath(name string, allowRoot bool) (string, error) {
	if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) || strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("%w: malformed relative path", ErrInvalidPath)
	}

	if strings.HasPrefix(name, "/") || hasWindowsVolume(name) {
		return "", fmt.Errorf("%w: absolute path", ErrOutsideRoot)
	}

	for segment := range strings.SplitSeq(name, "/") {
		if segment == ".." {
			return "", fmt.Errorf("%w: parent traversal", ErrOutsideRoot)
		}
	}

	cleaned := path.Clean(name)
	if cleaned == "." && !allowRoot {
		return "", fmt.Errorf("%w: root is not a file path", ErrInvalidPath)
	}

	if !fs.ValidPath(cleaned) {
		return "", fmt.Errorf("%w: malformed relative path", ErrInvalidPath)
	}

	return cleaned, nil
}

// Open opens name through the confined root. Internal symbolic links are
// allowed only when their targets remain inside the root.
func (t *Tree) Open(name string) (*os.File, error) {
	normalized, err := NormalizePath(name, true)
	if err != nil {
		return nil, err
	}

	if t == nil {
		return nil, ErrClosed
	}

	t.lifecycle.RLock()
	defer t.lifecycle.RUnlock()

	if err := t.checkOpen(); err != nil {
		return nil, err
	}

	file, err := t.root.Open(filepath.FromSlash(normalized))
	if err != nil {
		return nil, fmt.Errorf("coding workspace: open %q: %w", normalized, err)
	}

	return file, nil
}

// Stat returns information about name, following internal symbolic links.
func (t *Tree) Stat(name string) (fs.FileInfo, error) {
	return t.stat(name, true)
}

// Lstat returns information about name without following its final symbolic
// link.
func (t *Tree) Lstat(name string) (fs.FileInfo, error) {
	return t.stat(name, false)
}

// Readlink returns the unexpanded target of a workspace symbolic link.
func (t *Tree) Readlink(name string) (string, error) {
	normalized, err := NormalizePath(name, false)
	if err != nil {
		return "", err
	}

	if t == nil {
		return "", ErrClosed
	}

	t.lifecycle.RLock()
	defer t.lifecycle.RUnlock()

	if err := t.checkOpen(); err != nil {
		return "", err
	}

	target, err := t.root.Readlink(filepath.FromSlash(normalized))
	if err != nil {
		return "", fmt.Errorf("coding workspace: read symbolic link %q: %w", normalized, err)
	}

	return target, nil
}

// ReadDir reads one directory without recursively following its entries.
func (t *Tree) ReadDir(name string) ([]fs.DirEntry, error) {
	file, err := t.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("coding workspace: read directory %q: %w", name, err)
	}

	return entries, nil
}

// FileSystem returns a confined read-only fs.FS view. Operations continue to
// honor Tree closure and path validation.
func (t *Tree) FileSystem() fs.FS {
	return treeFS{tree: t}
}

// InspectMutationPath validates a file mutation target. Existing path
// components may not be symbolic links; parents must be directories. A nil
// FileInfo means the final path does not exist.
func (t *Tree) InspectMutationPath(name string) (string, fs.FileInfo, error) {
	normalized, err := NormalizePath(name, false)
	if err != nil {
		return "", nil, err
	}

	components := strings.Split(normalized, "/")
	for index := range components {
		current := strings.Join(components[:index+1], "/")

		info, statErr := t.Lstat(current)
		if errors.Is(statErr, fs.ErrNotExist) && index == len(components)-1 {
			return normalized, nil, nil
		}

		if statErr != nil {
			return "", nil, statErr
		}

		if info.Mode()&fs.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("%w: %q", ErrSymlink, current)
		}

		if index < len(components)-1 && !info.IsDir() {
			return "", nil, fmt.Errorf("%w: parent %q is not a directory", ErrUnsupportedType, current)
		}

		if index == len(components)-1 {
			if !info.Mode().IsRegular() {
				return "", nil, fmt.Errorf("%w: %q", ErrUnsupportedType, normalized)
			}

			return normalized, info, nil
		}
	}

	return "", nil, fmt.Errorf("%w: empty mutation path", ErrInvalidPath)
}

// Mutate serializes a guarded filesystem mutation. The callback may open
// stable directory handles and operate on base names through Mutation.
func (t *Tree) Mutate(ctx context.Context, fn func(*Mutation) error) error {
	if fn == nil {
		return errors.New("coding workspace: nil mutation")
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if t == nil {
		return ErrClosed
	}

	t.mutation.Lock()
	defer t.mutation.Unlock()

	t.lifecycle.RLock()
	defer t.lifecycle.RUnlock()

	if err := t.checkOpen(); err != nil {
		return err
	}

	state := &mutationState{active: true}

	mutation := &Mutation{root: t.root, state: state}
	defer func() { state.active = false }()

	return fn(mutation)
}

func (t *Tree) stat(name string, follow bool) (fs.FileInfo, error) {
	normalized, err := NormalizePath(name, true)
	if err != nil {
		return nil, err
	}

	if t == nil {
		return nil, ErrClosed
	}

	t.lifecycle.RLock()
	defer t.lifecycle.RUnlock()

	if err := t.checkOpen(); err != nil {
		return nil, err
	}

	local := filepath.FromSlash(normalized)

	var info fs.FileInfo
	if follow {
		info, err = t.root.Stat(local)
	} else {
		info, err = t.root.Lstat(local)
	}

	if err != nil {
		return nil, fmt.Errorf("coding workspace: inspect %q: %w", normalized, err)
	}

	return info, nil
}

func (t *Tree) checkOpen() error {
	if t == nil || t.root == nil || t.closed {
		return ErrClosed
	}

	return nil
}

type treeFS struct {
	tree *Tree
}

func (f treeFS) Open(name string) (fs.File, error) {
	return f.tree.Open(name)
}

func hasWindowsVolume(name string) bool {
	return len(name) >= 2 && name[1] == ':' &&
		((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z'))
}

// Mutation is active only for the duration of a Tree.Mutate callback.
type Mutation struct {
	root  *os.Root
	state *mutationState
}

type mutationState struct {
	active bool
}

// OpenDir opens a stable, non-symlink directory beneath the workspace.
//
//nolint:gocyclo // Component checks and handle identity verification form one security boundary.
func (m *Mutation) OpenDir(name string) (*MutationDir, error) {
	if err := m.check(); err != nil {
		return nil, err
	}

	normalized, err := NormalizePath(name, true)
	if err != nil {
		return nil, err
	}

	components := strings.Split(normalized, "/")
	if normalized == "." {
		components = nil
	}

	var expected fs.FileInfo

	for index := range components {
		current := strings.Join(components[:index+1], "/")

		expected, err = m.root.Lstat(filepath.FromSlash(current))
		if err != nil {
			return nil, fmt.Errorf("coding workspace: inspect mutation directory %q: %w", current, err)
		}

		if expected.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %q", ErrSymlink, current)
		}

		if !expected.IsDir() {
			return nil, fmt.Errorf("%w: %q is not a directory", ErrUnsupportedType, current)
		}
	}

	if normalized == "." {
		expected, err = m.root.Lstat(".")
		if err != nil {
			return nil, fmt.Errorf("coding workspace: inspect mutation root: %w", err)
		}
	}

	root, err := m.root.OpenRoot(filepath.FromSlash(normalized))
	if err != nil {
		return nil, fmt.Errorf("coding workspace: open mutation directory %q: %w", normalized, err)
	}

	opened, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("coding workspace: inspect opened mutation directory %q: %w", normalized, err)
	}

	if !opened.IsDir() || !os.SameFile(expected, opened) {
		_ = root.Close()
		return nil, fmt.Errorf("%w: mutation directory %q", ErrChanged, normalized)
	}

	return &MutationDir{root: root, path: normalized, state: m.state}, nil
}

func (m *Mutation) check() error {
	if m == nil || m.state == nil || !m.state.active || m.root == nil {
		return ErrClosed
	}

	return nil
}

// MutationDir is a stable directory handle used by a guarded mutation.
type MutationDir struct {
	root   *os.Root
	path   string
	state  *mutationState
	closed bool
}

// Path returns the normalized workspace-relative directory path.
func (d *MutationDir) Path() string {
	if d == nil {
		return ""
	}

	return d.path
}

// Close releases the directory handle. Repeated calls are safe.
func (d *MutationDir) Close() error {
	if d == nil || d.closed {
		return nil
	}

	d.closed = true
	if err := d.root.Close(); err != nil {
		return fmt.Errorf("coding workspace: close mutation directory %q: %w", d.path, err)
	}

	return nil
}

// Lstat inspects a direct child without following a final symbolic link.
func (d *MutationDir) Lstat(name string) (fs.FileInfo, error) {
	if err := d.checkBase(name); err != nil {
		return nil, err
	}

	info, err := d.root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("coding workspace: inspect mutation target %q: %w", d.display(name), err)
	}

	return info, nil
}

// Open opens a direct child for guarded inspection.
func (d *MutationDir) Open(name string) (*os.File, error) {
	if err := d.checkBase(name); err != nil {
		return nil, err
	}

	file, err := d.root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("coding workspace: open mutation target %q: %w", d.display(name), err)
	}

	return file, nil
}

// OpenFile opens a direct child using the supplied flags and permission bits.
func (d *MutationDir) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	if err := d.checkBase(name); err != nil {
		return nil, err
	}

	file, err := d.root.OpenFile(name, flag, perm.Perm())
	if err != nil {
		return nil, fmt.Errorf("coding workspace: open mutation target %q: %w", d.display(name), err)
	}

	return file, nil
}

// Link creates a hard link between two direct children. It fails when the new
// name already exists, which makes it suitable for guarded backup creation.
func (d *MutationDir) Link(oldName, newName string) error {
	if err := d.checkBase(oldName); err != nil {
		return err
	}

	if err := d.checkBase(newName); err != nil {
		return err
	}

	if err := d.root.Link(oldName, newName); err != nil {
		return fmt.Errorf(
			"coding workspace: link mutation target %q to %q: %w",
			d.display(oldName),
			d.display(newName),
			err,
		)
	}

	return nil
}

// Rename atomically renames one direct child to another in the same directory.
func (d *MutationDir) Rename(oldName, newName string) error {
	if err := d.checkBase(oldName); err != nil {
		return err
	}

	if err := d.checkBase(newName); err != nil {
		return err
	}

	if err := d.root.Rename(oldName, newName); err != nil {
		return fmt.Errorf(
			"coding workspace: rename mutation target %q to %q: %w",
			d.display(oldName),
			d.display(newName),
			err,
		)
	}

	return nil
}

// Remove removes one direct child.
func (d *MutationDir) Remove(name string) error {
	if err := d.checkBase(name); err != nil {
		return err
	}

	if err := d.root.Remove(name); err != nil {
		return fmt.Errorf("coding workspace: remove mutation target %q: %w", d.display(name), err)
	}

	return nil
}

// Sync asks the operating system to persist directory metadata.
func (d *MutationDir) Sync() error {
	if err := d.check(); err != nil {
		return err
	}

	file, err := d.root.Open(".")
	if err != nil {
		return fmt.Errorf("coding workspace: open mutation directory %q for sync: %w", d.path, err)
	}

	syncErr := file.Sync()

	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}

	return nil
}

func (d *MutationDir) checkBase(name string) error {
	if err := d.check(); err != nil {
		return err
	}

	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("%w: invalid base name", ErrInvalidPath)
	}

	return nil
}

func (d *MutationDir) check() error {
	if d == nil || d.root == nil || d.state == nil || !d.state.active || d.closed {
		return ErrClosed
	}

	return nil
}

func (d *MutationDir) display(name string) string {
	if d.path == "." {
		return name
	}

	return d.path + "/" + name
}
