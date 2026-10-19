//nolint:wsl_v5 // Group record inspection and its safety checks stay adjacent.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	workspaceSchema        = "pips.coding.session.workspace/v1"
	maxWorkspaceRecordSize = 64 << 10
)

// WorkspaceRecord identifies one workspace group directory.
type WorkspaceRecord struct {
	Schema    string    `json:"schema"`
	Key       string    `json:"key"`
	Path      string    `json:"path,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Group is one resolved workspace group.
type Group struct {
	// Key is the group directory name.
	Key string
	// Dir is the absolute group directory.
	Dir string
	// Path is the original workspace path, empty when the group has no readable
	// workspace record.
	Path string
}

// SessionDirOf returns the absolute session directory inside one group.
func (g Group) SessionDirOf(id string) string { return filepath.Join(g.Dir, id) }

// StoreRoot is the session store on disk: the root directory plus the
// operations that read or create the tree beneath it. [StoreLayout] says where
// everything goes; this is the type that touches the filesystem to find it.
type StoreRoot struct {
	layout StoreLayout
}

// NewStoreRoot returns the store rooted exactly at root. It reads and creates
// nothing.
func NewStoreRoot(root string) (StoreRoot, error) {
	layout, err := NewStoreLayout(root)
	if err != nil {
		return StoreRoot{}, err
	}

	return StoreRoot{layout: layout}, nil
}

// Layout returns the pure path scheme of this store.
func (s StoreRoot) Layout() StoreLayout { return s.layout }

// Root returns the absolute store root.
func (s StoreRoot) Root() string { return s.layout.Root() }

// EnsureGroup creates the group directory and its workspace record.
func (s StoreRoot) EnsureGroup(workspacePath string) (Group, error) {
	key, _, err := s.layout.GroupKey(workspacePath)
	if err != nil {
		return Group{}, err
	}

	dir := filepath.Join(s.layout.Root(), key)
	if err := ensureSessionDirectory(dir); err != nil {
		return Group{}, err
	}

	trimmed := strings.TrimSpace(workspacePath)
	record := WorkspaceRecord{
		Schema: workspaceSchema, Key: key, CreatedAt: time.Now().UTC(),
	}
	if trimmed != "" {
		record.Path = filepath.Clean(trimmed)
	}

	path := filepath.Join(dir, WorkspaceFileName)
	if _, statErr := os.Lstat(path); statErr == nil {
		// An existing record is authoritative for the original path; refresh only
		// when the caller now knows a path the record lacks.
		if existing, readErr := readWorkspaceRecord(path); readErr == nil && existing.Path != "" {
			record = existing
		} else if writeErr := writeWorkspaceRecord(path, record); writeErr != nil {
			return Group{}, writeErr
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Group{}, fmt.Errorf("coding session: inspect workspace record: %w", statErr)
	} else if err := writeWorkspaceRecord(path, record); err != nil {
		return Group{}, err
	}

	return Group{Key: key, Dir: dir, Path: record.Path}, nil
}

// Groups returns every workspace group in the store, sorted by key.
func (s StoreRoot) Groups() ([]Group, error) {
	root := s.layout.Root()

	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("coding session: list store groups: %w", err)
	}

	groups := make([]Group, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}

		group := Group{Key: entry.Name(), Dir: filepath.Join(root, entry.Name())}
		if record, readErr := readWorkspaceRecord(filepath.Join(group.Dir, WorkspaceFileName)); readErr == nil {
			group.Path = record.Path
		}

		groups = append(groups, group)
	}

	return groups, nil
}

// FindSession locates one session directory by id across every group. It
// validates the id and reports false when no group holds it.
func (s StoreRoot) FindSession(id string) (Group, bool, error) {
	if err := validateSessionID(id); err != nil {
		return Group{}, false, err
	}

	groups, err := s.Groups()
	if err != nil {
		return Group{}, false, err
	}
	for _, group := range groups {
		record := filepath.Join(group.Dir, id, MetadataFileName)
		if info, statErr := os.Lstat(record); statErr == nil && info.Mode().IsRegular() {
			return group, true, nil
		}
	}

	return Group{}, false, nil
}

func readWorkspaceRecord(path string) (WorkspaceRecord, error) {
	file, err := os.Open(path) //nolint:gosec // the caller owns the store root
	if err != nil {
		return WorkspaceRecord{}, err
	}
	defer func() { _ = file.Close() }()

	data, err := readBoundedFile(file, maxWorkspaceRecordSize)
	if err != nil {
		return WorkspaceRecord{}, err
	}

	var record WorkspaceRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return WorkspaceRecord{}, fmt.Errorf("coding session: decode workspace record: %w", err)
	}
	if record.Schema != workspaceSchema || record.Key == "" {
		return WorkspaceRecord{}, fmt.Errorf("%w: unsupported workspace record", ErrLayout)
	}

	return record, nil
}

func writeWorkspaceRecord(path string, record WorkspaceRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("coding session: encode workspace record: %w", err)
	}

	return writeFileAtomic(path, append(encoded, '\n'), 0o600)
}
