//nolint:wsl_v5 // Path construction and its bounds checks stay adjacent.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Session store v2 layout. One session is one directory under a workspace
// group; every durable artifact of that session lives inside it.
const (
	// StoreSchema names the on-disk session store format.
	StoreSchema = "pips.coding.session/v2"
	// StoreVersion is the numeric store format version.
	StoreVersion = 2

	// MetadataFileName is the session index record inside a session directory.
	MetadataFileName = "session.json"
	// TranscriptFileName is the authoritative conversation tree.
	TranscriptFileName = "transcript.jsonl"
	// JournalFileName is the authoritative coding event journal.
	JournalFileName = "events.jsonl"
	// SearchFileName is the derived, rebuildable search document.
	SearchFileName = "search.json"
	// WorkspaceFileName records a group's original workspace path.
	WorkspaceFileName = "workspace.json"
	// LockFileName is the single-writer advisory lock inside a session directory.
	LockFileName = "session.lock"

	// ModelDirName holds model-input snapshots.
	ModelDirName = "model"
	// StateDirName holds derived, rebuildable state sidecars.
	StateDirName = "state"
	// ArchiveDirName holds content-addressed archive objects.
	ArchiveDirName = "archives"
	// CompactionDirName holds compaction checkpoints.
	CompactionDirName = "compaction"
	// SubagentsDirName holds the parent-side child index.
	SubagentsDirName = "subagents"
	// SubagentIndexFileName is the child index's completeness marker.
	SubagentIndexFileName = "index.json"
	// ChildIndexEntryFileName is one child's record inside the parent's index.
	ChildIndexEntryFileName = "entry.json"
	// TerminalDirName holds bounded tool output logs.
	TerminalDirName = "terminal"
	// ImagesDirName holds session media originals (clipboard, Workspace, ACP,
	// and future generated media).
	ImagesDirName = "images"
	// AssetsDirName holds the bounded model-ready copies of session media.
	AssetsDirName = "assets"

	// UnknownWorkspaceKey groups sessions whose workspace path is unknown.
	UnknownWorkspaceKey = "_unknown"

	maxWorkspaceKeyBytes   = 255
	workspaceFallbackBytes = 16
)

// ErrLayout identifies an unusable session store layout input.
var ErrLayout = errors.New("coding session: invalid store layout")

// StoreLayout resolves every on-disk path of the layered session store rooted
// at one directory. It is a pure value: it holds the path scheme and never
// reads or creates anything. Use [StoreRoot] to find or create what the scheme
// describes.
type StoreLayout struct {
	root string
}

// NewStoreLayout returns a layout rooted exactly at root.
func NewStoreLayout(root string) (StoreLayout, error) {
	if strings.TrimSpace(root) == "" || strings.ContainsRune(root, '\x00') {
		return StoreLayout{}, fmt.Errorf("%w: empty or invalid store root", ErrLayout)
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return StoreLayout{}, fmt.Errorf("%w: absolute store root: %w", ErrLayout, err)
	}

	return StoreLayout{root: filepath.Clean(abs)}, nil
}

// Root returns the absolute store root.
func (l StoreLayout) Root() string { return l.root }

// GroupKey returns the group directory name for a workspace path.
//
// The key is the URL-escaped cleaned path when it fits in a single filesystem
// name; otherwise it is a bounded fallback derived from the path digest and the
// caller must persist the original path with [StoreRoot.EnsureGroup].
func (l StoreLayout) GroupKey(workspacePath string) (key string, fallback bool, err error) {
	trimmed := strings.TrimSpace(workspacePath)
	if trimmed == "" {
		return UnknownWorkspaceKey, true, nil
	}
	if strings.ContainsRune(trimmed, '\x00') {
		return "", false, fmt.Errorf("%w: workspace path contains NUL", ErrLayout)
	}

	cleaned := filepath.Clean(trimmed)
	escaped := url.PathEscape(cleaned)
	if len(escaped) <= maxWorkspaceKeyBytes {
		return escaped, false, nil
	}

	sum := sha256.Sum256([]byte(cleaned))

	return "ws-" + hex.EncodeToString(sum[:])[:workspaceFallbackBytes], true, nil
}

// GroupDir returns the absolute group directory for a workspace path.
func (l StoreLayout) GroupDir(workspacePath string) string {
	key, _, err := l.GroupKey(workspacePath)
	if err != nil || key == "" {
		return filepath.Join(l.root, UnknownWorkspaceKey)
	}

	return filepath.Join(l.root, key)
}

// SessionDir returns the absolute directory of one session.
func (l StoreLayout) SessionDir(workspacePath, id string) string {
	return filepath.Join(l.GroupDir(workspacePath), id)
}

// MetadataFile returns the session index record path.
func (l StoreLayout) MetadataFile(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), MetadataFileName)
}

// TranscriptFile returns the authoritative transcript path.
func (l StoreLayout) TranscriptFile(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), TranscriptFileName)
}

// JournalFile returns the coding event journal path.
func (l StoreLayout) JournalFile(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), JournalFileName)
}

// SearchFile returns the derived search document path.
func (l StoreLayout) SearchFile(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), SearchFileName)
}

// LockFile returns the single-writer lock path.
func (l StoreLayout) LockFile(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), LockFileName)
}

// ModelDir returns the model-input snapshot directory.
func (l StoreLayout) ModelDir(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), ModelDirName)
}

// StateDir returns the derived state directory.
func (l StoreLayout) StateDir(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), StateDirName)
}

// ArchiveDir returns the content-addressed archive directory.
func (l StoreLayout) ArchiveDir(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), ArchiveDirName)
}

// CompactionDir returns the compaction checkpoint directory.
func (l StoreLayout) CompactionDir(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), CompactionDirName)
}

// SubagentsDir returns the parent-side child index directory.
func (l StoreLayout) SubagentsDir(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), SubagentsDirName)
}

// SubagentIndexFile returns the child index's completeness marker path.
func (l StoreLayout) SubagentIndexFile(workspacePath, id string) string {
	return filepath.Join(l.SubagentsDir(workspacePath, id), SubagentIndexFileName)
}

// ChildIndexEntryDir returns the directory of one indexed child.
func (l StoreLayout) ChildIndexEntryDir(workspacePath, parentID, childID string) string {
	return filepath.Join(l.SubagentsDir(workspacePath, parentID), childID)
}

// ChildIndexEntryFile returns the index entry path of one indexed child.
func (l StoreLayout) ChildIndexEntryFile(workspacePath, parentID, childID string) string {
	return filepath.Join(l.ChildIndexEntryDir(workspacePath, parentID, childID), ChildIndexEntryFileName)
}

// TerminalDir returns the tool output log directory.
func (l StoreLayout) TerminalDir(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), TerminalDirName)
}

// ImagesDir returns the session media originals directory.
func (l StoreLayout) ImagesDir(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), ImagesDirName)
}

// AssetsDir returns the bounded model-ready media copy directory.
func (l StoreLayout) AssetsDir(workspacePath, id string) string {
	return filepath.Join(l.SessionDir(workspacePath, id), AssetsDirName)
}

// SessionRelDir returns the session directory relative to the store root, split
// into single path components. Callers that must stay descriptor-relative walk
// these components one at a time.
func (l StoreLayout) SessionRelDir(workspacePath, id string) []string {
	key, _, err := l.GroupKey(workspacePath)
	if err != nil || key == "" {
		key = UnknownWorkspaceKey
	}

	return []string{key, id}
}

// ArchiveRelDir returns the archive directory relative to the store root as
// single path components.
func (l StoreLayout) ArchiveRelDir(workspacePath, id string) []string {
	return append(l.SessionRelDir(workspacePath, id), ArchiveDirName)
}
