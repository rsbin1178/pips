package workspace

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/jsonx"
)

const (
	// StoreSchema identifies the durable workspace store format.
	StoreSchema = "pips.workspaces/v1alpha1"

	maxStoreBytes     = 4 << 20
	maxWorkspaceCount = 10_000
	maxWorkspacePath  = 32 << 10
)

var (
	// ErrInsecurePermissions means a workspace store path can be accessed by
	// users other than its owner.
	ErrInsecurePermissions = errors.New("coding workspace: insecure store permissions")
	// ErrUnsupportedStoreSchema means the store uses an unknown schema.
	ErrUnsupportedStoreSchema = errors.New("coding workspace: unsupported store schema")
	// ErrStoreTooLarge means the store exceeds its bounded input size.
	ErrStoreTooLarge = errors.New("coding workspace: store too large")
	// ErrWorkspaceUnknown means a permission was recorded before its Workspace
	// was explicitly trusted.
	ErrWorkspaceUnknown = errors.New("coding workspace: workspace is not trusted")
)

// PermissionKind identifies a typed workspace-scoped authority.
type PermissionKind string

// Supported permission kinds.
const (
	PermissionMCPServer PermissionKind = "mcp_server"
)

// PermissionDecision is an explicit local allow or deny decision.
type PermissionDecision string

// Supported permission decisions.
const (
	PermissionAllow PermissionDecision = "allow"
	PermissionDeny  PermissionDecision = "deny"
)

// Permission binds one local decision to a normalized resource fingerprint.
type Permission struct {
	Kind        PermissionKind     `json:"kind"`
	ResourceID  string             `json:"resource_id"`
	Fingerprint string             `json:"fingerprint"`
	Decision    PermissionDecision `json:"decision"`
	DecidedAt   time.Time          `json:"decided_at"`
}

// Store records workspace-scoped user decisions outside project directories.
// The file is read lazily and mutations are serialized per Store instance.
type Store struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

// NewStore returns a workspace store backed by path.
func NewStore(path string) *Store {
	return &Store{path: path, now: time.Now}
}

// Path returns the workspace store path.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}

	return s.path
}

// IsTrusted reports whether identity is present and still matches its stored
// canonical path and filesystem identity.
func (s *Store) IsTrusted(identity Identity) (bool, error) {
	if err := s.validate(identity); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, exists, err := s.load()
	if err != nil || !exists {
		return false, err
	}

	record, ok := data.Workspaces[identity.Key()]
	if !ok {
		return false, nil
	}

	return record.Path == identity.Path() &&
		record.Device == identity.Device() &&
		record.Inode == identity.Inode(), nil
}

// Trust records an explicit trust decision for identity.
func (s *Store) Trust(identity Identity) error {
	if err := s.validate(identity); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, exists, err := s.load()
	if err != nil {
		return err
	}

	if !exists {
		data = storeFile{
			Schema:     StoreSchema,
			Workspaces: make(map[string]workspaceRecord),
		}
	}

	if data.Workspaces == nil {
		data.Workspaces = make(map[string]workspaceRecord)
	}

	data.Workspaces[identity.Key()] = workspaceRecord{
		Path:      identity.Path(),
		Device:    identity.Device(),
		Inode:     identity.Inode(),
		TrustedAt: s.now().UTC(),
	}

	return s.write(data)
}

// SetPermission records a typed decision for an already trusted Workspace.
func (s *Store) SetPermission(identity Identity, permission Permission) error {
	if err := s.validate(identity); err != nil {
		return err
	}

	permission.DecidedAt = permission.DecidedAt.UTC()
	if err := validatePermission(permission); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, exists, err := s.load()
	if err != nil {
		return err
	}

	if !exists {
		return ErrWorkspaceUnknown
	}

	record, exists := data.Workspaces[identity.Key()]
	if !exists || record.Path != identity.Path() ||
		record.Device != identity.Device() || record.Inode != identity.Inode() {
		return ErrWorkspaceUnknown
	}

	replaced := false

	for index := range record.Permissions {
		current := record.Permissions[index]
		if current.Kind == permission.Kind && current.ResourceID == permission.ResourceID {
			record.Permissions[index] = permission
			replaced = true

			break
		}
	}

	if !replaced {
		record.Permissions = append(record.Permissions, permission)
	}

	slices.SortFunc(record.Permissions, comparePermissions)
	data.Workspaces[identity.Key()] = record

	return s.write(data)
}

// Permission returns one typed decision for identity and resourceID.
func (s *Store) Permission(
	identity Identity,
	kind PermissionKind,
	resourceID string,
) (Permission, bool, error) {
	if err := s.validate(identity); err != nil {
		return Permission{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, exists, err := s.load()
	if err != nil || !exists {
		return Permission{}, false, err
	}

	record, exists := data.Workspaces[identity.Key()]
	if !exists || record.Path != identity.Path() ||
		record.Device != identity.Device() || record.Inode != identity.Inode() {
		return Permission{}, false, nil
	}

	for _, permission := range record.Permissions {
		if permission.Kind == kind && permission.ResourceID == resourceID {
			return permission, true, nil
		}
	}

	return Permission{}, false, nil
}

func (s *Store) validate(identity Identity) error {
	if s == nil || s.path == "" {
		return fmt.Errorf("%w: empty workspace store path", ErrInvalid)
	}

	if !identity.valid() {
		return fmt.Errorf("%w: invalid filesystem identity", ErrInvalid)
	}

	return nil
}

type storeFile struct {
	Schema     string                     `json:"schema"`
	Workspaces map[string]workspaceRecord `json:"workspaces"`
}

type workspaceRecord struct {
	Path        string       `json:"path"`
	Device      uint64       `json:"device"`
	Inode       uint64       `json:"inode"`
	TrustedAt   time.Time    `json:"trusted_at"`
	Permissions []Permission `json:"permissions,omitempty"`
}

func (s *Store) load() (storeFile, bool, error) {
	var data storeFile

	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return data, false, nil
	}

	if err != nil {
		return data, false, fmt.Errorf("coding workspace: inspect store: %w", err)
	}

	if err := validateStoreInfo(s.path, info); err != nil {
		return data, false, err
	}

	file, err := os.Open(s.path)
	if err != nil {
		return data, false, fmt.Errorf("coding workspace: open store: %w", err)
	}
	defer func() { _ = file.Close() }()

	openedInfo, err := file.Stat()
	if err != nil {
		return data, false, fmt.Errorf("coding workspace: inspect opened store: %w", err)
	}

	if !os.SameFile(info, openedInfo) {
		return data, false, errors.New("coding workspace: store changed while opening")
	}

	encoded, err := io.ReadAll(io.LimitReader(file, maxStoreBytes+1))
	if err != nil {
		return data, false, fmt.Errorf("coding workspace: read store: %w", err)
	}

	if len(encoded) > maxStoreBytes {
		return data, false, ErrStoreTooLarge
	}

	if err := jsonx.Decode(encoded, &data); err != nil {
		return data, false, fmt.Errorf("coding workspace: decode store: %w", err)
	}

	if err := validateStoreFile(data); err != nil {
		return data, false, err
	}

	return data, true, nil
}

func validateStoreInfo(storePath string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("coding workspace: store is not a regular file")
	}

	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf(
			"%w: file %q has mode %04o",
			ErrInsecurePermissions,
			storePath,
			info.Mode().Perm(),
		)
	}

	return nil
}

func validateStoreFile(data storeFile) error {
	if data.Schema != StoreSchema {
		return fmt.Errorf("%w: %q", ErrUnsupportedStoreSchema, data.Schema)
	}

	if data.Workspaces == nil {
		return errors.New("coding workspace: store workspaces must be an object")
	}

	if len(data.Workspaces) > maxWorkspaceCount {
		return errors.New("coding workspace: too many workspace records")
	}

	for key, record := range data.Workspaces {
		if err := validateWorkspaceRecord(key, record); err != nil {
			return err
		}
	}

	return nil
}

func validateWorkspaceRecord(key string, record workspaceRecord) error {
	if len(record.Path) > maxWorkspacePath ||
		!filepath.IsAbs(record.Path) || filepath.Clean(record.Path) != record.Path {
		return fmt.Errorf("coding workspace: invalid stored path for %q", key)
	}

	if record.TrustedAt.IsZero() {
		return fmt.Errorf("coding workspace: missing trust timestamp for %q", key)
	}

	_, offset := record.TrustedAt.Zone()
	if offset != 0 {
		return fmt.Errorf("coding workspace: trust timestamp is not UTC for %q", key)
	}

	identity := Identity{
		path:   record.Path,
		device: record.Device,
		inode:  record.Inode,
		set:    true,
	}
	if identity.Key() != key {
		return fmt.Errorf("coding workspace: record identity does not match key %q", key)
	}

	seen := make(map[string]struct{}, len(record.Permissions))
	for index, permission := range record.Permissions {
		if err := validatePermission(permission); err != nil {
			return fmt.Errorf("coding workspace: permission %d for %q: %w", index, key, err)
		}

		permissionKey := string(permission.Kind) + "\x00" + permission.ResourceID
		if _, duplicate := seen[permissionKey]; duplicate {
			return fmt.Errorf("coding workspace: duplicate permission for %q", key)
		}

		seen[permissionKey] = struct{}{}
	}

	return nil
}

func validatePermission(permission Permission) error {
	if permission.Kind != PermissionMCPServer {
		return errors.New("unsupported permission kind")
	}

	if !validPermissionResourceID(permission.ResourceID) {
		return errors.New("invalid permission resource ID")
	}

	decoded, err := hex.DecodeString(permission.Fingerprint)
	if err != nil || len(decoded) != 32 || strings.ToLower(permission.Fingerprint) != permission.Fingerprint {
		return errors.New("invalid permission fingerprint")
	}

	if permission.Decision != PermissionAllow && permission.Decision != PermissionDeny {
		return errors.New("invalid permission decision")
	}

	if permission.DecidedAt.IsZero() {
		return errors.New("missing permission timestamp")
	}

	_, offset := permission.DecidedAt.Zone()
	if offset != 0 {
		return errors.New("permission timestamp is not UTC")
	}

	return nil
}

func validPermissionResourceID(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}

	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}

	return true
}

func comparePermissions(left, right Permission) int {
	if compared := strings.Compare(string(left.Kind), string(right.Kind)); compared != 0 {
		return compared
	}

	return strings.Compare(left.ResourceID, right.ResourceID)
}

func (s *Store) write(data storeFile) error {
	if err := validateStoreFile(data); err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := secureDirectory(dir); err != nil {
		return err
	}

	temp, err := os.CreateTemp(dir, ".workspaces-*")
	if err != nil {
		return fmt.Errorf("coding workspace: create temporary store: %w", err)
	}

	tempPath := temp.Name()
	removeTemp := true

	defer func() {
		_ = temp.Close()

		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("coding workspace: secure temporary store: %w", err)
	}

	encoder := json.NewEncoder(temp)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("coding workspace: encode store: %w", err)
	}

	if err := temp.Sync(); err != nil {
		return fmt.Errorf("coding workspace: sync store: %w", err)
	}

	if err := temp.Close(); err != nil {
		return fmt.Errorf("coding workspace: close store: %w", err)
	}

	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("coding workspace: replace store: %w", err)
	}

	removeTemp = false

	return nil
}

func secureDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("coding workspace: create store directory: %w", err)
	}

	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("coding workspace: inspect store directory: %w", err)
	}

	if !info.IsDir() {
		return errors.New("coding workspace: store parent is not a directory")
	}

	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf(
			"%w: directory %q has mode %04o",
			ErrInsecurePermissions,
			directory,
			info.Mode().Perm(),
		)
	}

	return nil
}
