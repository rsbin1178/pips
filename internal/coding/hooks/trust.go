package hooks

import (
	"context"
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

	"github.com/rsbin/pips/internal/jsonx"
)

// TrustStore records Pips-owned exact command-handler approvals outside a
// project-controlled hooks.json file.
type TrustStore struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

// NewTrustStore returns a hook trust store backed by path.
func NewTrustStore(path string) *TrustStore {
	return &TrustStore{path: path, now: time.Now}
}

// Path returns the backing trust store path.
func (s *TrustStore) Path() string {
	if s == nil {
		return ""
	}

	return s.path
}

// Resolve attaches the effective trust status to each definition. workspaceID
// is required only for project definitions and must be the current filesystem
// identity key, not a user-controlled path.
func (s *TrustStore) Resolve(
	ctx context.Context,
	definitions Definitions,
	workspaceID string,
) ([]ResolvedDefinition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.path == "" {
		return nil, fmt.Errorf("%w: empty trust store path", ErrInvalid)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, exists, err := s.load()
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{})
	if exists {
		for _, record := range data.Records {
			allowed[record.key()] = struct{}{}
		}
	}

	values := definitions.List()
	resolved := make([]ResolvedDefinition, 0, len(values))
	for _, definition := range values {
		key, keyErr := trustKey(definition, workspaceID)
		if keyErr != nil {
			return nil, keyErr
		}
		status := StatusPending
		if _, ok := allowed[key]; ok {
			status = StatusTrusted
		}
		resolved = append(resolved, ResolvedDefinition{Definition: definition, Status: status})
	}

	return resolved, nil
}

// Trust records approval for Definition's current semantic fingerprint.
func (s *TrustStore) Trust(
	ctx context.Context,
	definition Definition,
	workspaceID string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.path == "" {
		return fmt.Errorf("%w: empty trust store path", ErrInvalid)
	}
	if _, err := trustKey(definition, workspaceID); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, exists, err := s.load()
	if err != nil {
		return err
	}
	if !exists {
		data = trustFile{Schema: TrustSchema, Records: make([]trustRecord, 0, 1)}
	}

	record := trustRecord{
		Scope: definition.Scope, WorkspaceID: projectWorkspaceID(definition, workspaceID),
		Fingerprint: definition.Fingerprint(), TrustedAt: s.now().UTC(),
	}
	replaced := false
	for index := range data.Records {
		if data.Records[index].key() == record.key() {
			data.Records[index] = record
			replaced = true
			break
		}
	}
	if !replaced {
		data.Records = append(data.Records, record)
	}
	slices.SortFunc(data.Records, func(left, right trustRecord) int {
		return strings.Compare(left.key(), right.key())
	})

	return s.write(data)
}

type trustFile struct {
	Schema  string        `json:"schema"`
	Records []trustRecord `json:"records"`
}

type trustRecord struct {
	Scope       Scope     `json:"scope"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	Fingerprint string    `json:"fingerprint"`
	TrustedAt   time.Time `json:"trusted_at"`
}

func (r trustRecord) key() string {
	return string(r.Scope) + "\x00" + r.WorkspaceID + "\x00" + r.Fingerprint
}

func trustKey(definition Definition, workspaceID string) (string, error) {
	if err := validateDefinition(definition, DefaultLimits()); err != nil {
		return "", fmt.Errorf("%w: invalid definition: %w", ErrInvalid, err)
	}
	if definition.Scope == ScopeProject && workspaceID == "" {
		return "", fmt.Errorf("%w: project definition requires workspace identity", ErrUntrusted)
	}
	if definition.Scope == ScopeUser {
		workspaceID = ""
	}

	return string(definition.Scope) + "\x00" + workspaceID + "\x00" + definition.Fingerprint(), nil
}

func projectWorkspaceID(definition Definition, workspaceID string) string {
	if definition.Scope != ScopeProject {
		return ""
	}

	return workspaceID
}

func (s *TrustStore) load() (trustFile, bool, error) {
	var data trustFile
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return data, false, nil
	}
	if err != nil {
		return data, false, fmt.Errorf("coding hooks: inspect trust store: %w", err)
	}
	if err := validateTrustFileInfo(info); err != nil {
		return data, false, err
	}

	file, err := os.Open(s.path)
	if err != nil {
		return data, false, fmt.Errorf("coding hooks: open trust store: %w", err)
	}
	defer func() { _ = file.Close() }()

	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return data, false, fmt.Errorf("%w: trust store changed while opening", ErrUnsafeFile)
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maximumFileBytes+1))
	if err != nil {
		return data, false, fmt.Errorf("coding hooks: read trust store: %w", err)
	}
	if len(encoded) > maximumFileBytes {
		return data, false, ErrLimitExceeded
	}
	if err := jsonx.Decode(encoded, &data); err != nil {
		return data, false, fmt.Errorf("coding hooks: decode trust store: %w", err)
	}
	if err := validateTrustFile(data); err != nil {
		return data, false, err
	}

	return data, true, nil
}

func validateTrustFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: hook trust store", ErrUnsafeFile)
	}

	return nil
}

func validateTrustFile(data trustFile) error {
	if data.Schema != TrustSchema || data.Records == nil {
		return fmt.Errorf("%w: invalid trust schema or records", ErrInvalid)
	}
	if len(data.Records) > maximumTrustRecords {
		return ErrLimitExceeded
	}
	seen := make(map[string]struct{}, len(data.Records))
	for index, record := range data.Records {
		if err := validateTrustRecord(record); err != nil {
			return fmt.Errorf("%w: trust record %d: %w", ErrInvalid, index, err)
		}
		if _, duplicate := seen[record.key()]; duplicate {
			return fmt.Errorf("%w: duplicate trust record", ErrInvalid)
		}
		seen[record.key()] = struct{}{}
	}

	return nil
}

func validateTrustRecord(record trustRecord) error {
	if !record.Scope.valid() || len(record.Fingerprint) != sha256HexLength ||
		strings.ToLower(record.Fingerprint) != record.Fingerprint {
		return errors.New("invalid trust record identity")
	}
	decoded, err := hex.DecodeString(record.Fingerprint)
	if err != nil || len(decoded) != sha256Bytes {
		return errors.New("invalid trust fingerprint")
	}
	if record.Scope == ScopeProject && record.WorkspaceID == "" ||
		record.Scope == ScopeUser && record.WorkspaceID != "" {
		return errors.New("invalid trust workspace identity")
	}
	if record.TrustedAt.IsZero() {
		return errors.New("missing trust timestamp")
	}
	_, offset := record.TrustedAt.Zone()
	if offset != 0 {
		return errors.New("trust timestamp is not UTC")
	}

	return nil
}

const (
	sha256Bytes     = 32
	sha256HexLength = sha256Bytes * 2
)

func (s *TrustStore) write(data trustFile) error {
	if err := validateTrustFile(data); err != nil {
		return err
	}
	directory := filepath.Dir(s.path)
	if err := secureDirectory(directory); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(directory, ".hook-trust-*")
	if err != nil {
		return fmt.Errorf("coding hooks: create trust temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("coding hooks: secure trust temporary file: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("coding hooks: encode trust store: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("coding hooks: sync trust store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("coding hooks: close trust store: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("coding hooks: replace trust store: %w", err)
	}
	removeTemporary = false

	return nil
}

func secureDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("coding hooks: create trust directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("coding hooks: inspect trust directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: hook trust directory", ErrUnsafeFile)
	}

	return nil
}
