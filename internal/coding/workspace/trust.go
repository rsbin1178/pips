package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const trustStoreVersion = 1

var (
	// ErrInsecurePermissions means a trust store path can be accessed by
	// users other than its owner.
	ErrInsecurePermissions = errors.New("coding workspace: insecure trust store permissions")
	// ErrUnsupportedTrustVersion means the trust store was written with an
	// unknown schema version.
	ErrUnsupportedTrustVersion = errors.New("coding workspace: unsupported trust store version")
)

// TrustStore records explicit user trust outside a project directory.
type TrustStore struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

// NewTrustStore returns a trust store backed by path. The file is read lazily.
func NewTrustStore(path string) *TrustStore {
	return &TrustStore{path: path, now: time.Now}
}

// Path returns the trust store path.
func (s *TrustStore) Path() string {
	if s == nil {
		return ""
	}

	return s.path
}

// IsTrusted reports whether identity is present and still matches its stored
// canonical path and filesystem identity.
func (s *TrustStore) IsTrusted(identity Identity) (bool, error) {
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
func (s *TrustStore) Trust(identity Identity) error {
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
		data = trustFile{Version: trustStoreVersion, Workspaces: make(map[string]trustRecord)}
	}

	if data.Workspaces == nil {
		data.Workspaces = make(map[string]trustRecord)
	}

	data.Workspaces[identity.Key()] = trustRecord{
		Path:      identity.Path(),
		Device:    identity.Device(),
		Inode:     identity.Inode(),
		TrustedAt: s.now().UTC(),
	}

	return s.write(data)
}

func (s *TrustStore) validate(identity Identity) error {
	if s == nil || s.path == "" {
		return fmt.Errorf("%w: empty trust store path", ErrInvalid)
	}

	if !identity.valid() {
		return fmt.Errorf("%w: invalid filesystem identity", ErrInvalid)
	}

	return nil
}

type trustFile struct {
	Version    int                    `json:"version"`
	Workspaces map[string]trustRecord `json:"workspaces"`
}

type trustRecord struct {
	Path      string    `json:"path"`
	Device    uint64    `json:"device"`
	Inode     uint64    `json:"inode"`
	TrustedAt time.Time `json:"trusted_at"`
}

func (s *TrustStore) load() (trustFile, bool, error) {
	var data trustFile

	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return data, false, nil
	}

	if err != nil {
		return data, false, fmt.Errorf("coding workspace: inspect trust store: %w", err)
	}

	if !info.Mode().IsRegular() {
		return data, false, errors.New("coding workspace: trust store is not a regular file")
	}

	if info.Mode().Perm()&0o077 != 0 {
		return data, false, fmt.Errorf("%w: file %q has mode %04o", ErrInsecurePermissions, s.path, info.Mode().Perm())
	}

	file, err := os.Open(s.path)
	if err != nil {
		return data, false, fmt.Errorf("coding workspace: open trust store: %w", err)
	}
	defer func() { _ = file.Close() }()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&data); err != nil {
		return data, false, fmt.Errorf("coding workspace: decode trust store: %w", err)
	}

	if err := expectJSONEOF(decoder); err != nil {
		return data, false, err
	}

	if data.Version != trustStoreVersion {
		return data, false, fmt.Errorf("%w: %d", ErrUnsupportedTrustVersion, data.Version)
	}

	return data, true, nil
}

func expectJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("coding workspace: decode trust store: trailing JSON value")
		}

		return fmt.Errorf("coding workspace: decode trust store: %w", err)
	}

	return nil
}

func (s *TrustStore) write(data trustFile) error {
	dir := filepath.Dir(s.path)
	if err := secureDirectory(dir); err != nil {
		return err
	}

	temp, err := os.CreateTemp(dir, ".trust-*")
	if err != nil {
		return fmt.Errorf("coding workspace: create temporary trust store: %w", err)
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
		return fmt.Errorf("coding workspace: secure temporary trust store: %w", err)
	}

	encoder := json.NewEncoder(temp)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("coding workspace: encode trust store: %w", err)
	}

	if err := temp.Sync(); err != nil {
		return fmt.Errorf("coding workspace: sync trust store: %w", err)
	}

	if err := temp.Close(); err != nil {
		return fmt.Errorf("coding workspace: close trust store: %w", err)
	}

	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("coding workspace: replace trust store: %w", err)
	}

	removeTemp = false

	return nil
}

func secureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("coding workspace: create trust store directory: %w", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("coding workspace: inspect trust store directory: %w", err)
	}

	if !info.IsDir() {
		return errors.New("coding workspace: trust store parent is not a directory")
	}

	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: directory %q has mode %04o", ErrInsecurePermissions, path, info.Mode().Perm())
	}

	return nil
}
