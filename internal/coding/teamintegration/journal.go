//nolint:wsl_v5 // Durable WAL validation keeps each identity and commit-marker check adjacent.
package teamintegration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rsbin/pips/internal/jsonx"
)

const (
	journalSchema       = "pips.coding.team-integration-journal/v1alpha1"
	journalFileName     = "journal.json"
	maximumJournalBytes = 8 << 20
)

type journalState string

const (
	journalApplying    journalState = "applying"
	journalInterrupted journalState = "interrupted"
	journalApplied     journalState = "applied"
	journalRolledBack  journalState = "rolled_back"
)

type applyJournal struct {
	Schema            string       `json:"schema"`
	ID                string       `json:"id"`
	WorkspacePath     string       `json:"workspace_path"`
	WorkspaceIdentity string       `json:"workspace_identity"`
	CommonIdentity    string       `json:"common_identity"`
	BranchRef         string       `json:"branch_ref"`
	HeadOID           string       `json:"head_oid"`
	IndexDigest       string       `json:"index_digest"`
	IntegrationCommit string       `json:"integration_commit"`
	IntegrationTree   string       `json:"integration_tree"`
	Manifest          Manifest     `json:"manifest"`
	State             journalState `json:"state"`
	Completed         []string     `json:"completed,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

func (m *Manager) writeJournal(journal applyJournal) error {
	if err := validateJournal(journal); err != nil {
		return err
	}
	if err := validateManifestEvidence(journal.Manifest, m.limits); err != nil {
		return err
	}
	directory := filepath.Join(m.integrationsRoot.Root(), journal.ID)
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}

	encoded, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if len(encoded) > maximumJournalBytes {
		return ErrLimit
	}
	encoded = append(encoded, '\n')

	temporary, err := os.CreateTemp(directory, ".journal-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(privateFileMode); err != nil {
		_ = temporary.Close()

		return err
	}
	_, writeErr := temporary.Write(encoded)
	closeErr := errors.Join(temporary.Sync(), temporary.Close())
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	if err := os.Rename(temporaryPath, filepath.Join(directory, journalFileName)); err != nil {
		return err
	}
	removeTemporary = false

	return syncDirectory(directory)
}

//nolint:gocyclo // WAL reads validate identity, size, schema, trailing data, and digest together.
func (m *Manager) readJournal(id string) (applyJournal, error) {
	if !validIntegrationID(id) {
		return applyJournal{}, fmt.Errorf("%w: journal id", ErrInvalid)
	}
	root, err := os.OpenRoot(m.integrationsRoot.Root())
	if err != nil {
		return applyJournal{}, err
	}
	defer func() { _ = root.Close() }()
	directoryInfo, err := root.Lstat(id)
	if err != nil {
		return applyJournal{}, err
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&fs.ModeSymlink != 0 ||
		directoryInfo.Mode().Perm()&0o077 != 0 {
		return applyJournal{}, fmt.Errorf("%w: unsafe journal directory", ErrInvalid)
	}
	relative := filepath.Join(id, journalFileName)
	info, err := root.Lstat(relative)
	if err != nil {
		return applyJournal{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maximumJournalBytes {
		return applyJournal{}, fmt.Errorf("%w: unsafe journal file", ErrInvalid)
	}
	file, err := root.Open(relative)
	if err != nil {
		return applyJournal{}, err
	}
	defer func() { _ = file.Close() }()

	encoded, err := io.ReadAll(io.LimitReader(file, maximumJournalBytes+1))
	if err != nil {
		return applyJournal{}, err
	}
	if len(encoded) > maximumJournalBytes {
		return applyJournal{}, ErrLimit
	}

	var journal applyJournal
	if err := jsonx.Decode(encoded, &journal); err != nil {
		return applyJournal{}, err
	}
	if err := validateJournal(journal); err != nil {
		return applyJournal{}, err
	}
	if err := validateManifestEvidence(journal.Manifest, m.limits); err != nil {
		return applyJournal{}, err
	}

	return journal, nil
}

func (m *Manager) listJournals() ([]applyJournal, error) {
	entries, err := os.ReadDir(m.integrationsRoot.Root())
	if err != nil {
		return nil, err
	}
	if len(entries) > m.limits.Entries {
		return nil, ErrLimit
	}

	result := make([]applyJournal, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !validIntegrationID(entry.Name()) {
			continue
		}
		journal, err := m.readJournal(entry.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, journal)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ID < result[right].ID
	})

	return result, nil
}

//nolint:gocyclo // Persisted states and completed-path invariants form one strict replay boundary.
func validateJournal(journal applyJournal) error {
	if journal.Schema != journalSchema || !validIntegrationID(journal.ID) ||
		!filepath.IsAbs(journal.WorkspacePath) ||
		journal.WorkspaceIdentity == "" || journal.CommonIdentity == "" ||
		journal.BranchRef == "" || journal.HeadOID == "" || journal.IndexDigest == "" ||
		journal.IntegrationCommit == "" || journal.IntegrationTree == "" ||
		journal.Manifest.Digest == "" || journal.CreatedAt.IsZero() || journal.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: incomplete journal", ErrInvalid)
	}
	if err := validateManifestEvidence(journal.Manifest, DefaultLimits()); err != nil {
		return err
	}
	switch journal.State {
	case journalApplying, journalInterrupted, journalApplied, journalRolledBack:
	default:
		return fmt.Errorf("%w: journal state", ErrInvalid)
	}
	paths := SortedManifestPaths(journal.Manifest)
	if len(paths) != len(journal.Manifest.Entries) || len(paths) > maximumEntries {
		return ErrLimit
	}
	completed := make(map[string]struct{}, len(journal.Completed))
	for _, path := range journal.Completed {
		if _, exists := completed[path]; exists {
			return fmt.Errorf("%w: duplicate journal progress", ErrInvalid)
		}
		completed[path] = struct{}{}
		index := sort.SearchStrings(paths, path)
		if index == len(paths) || paths[index] != path {
			return fmt.Errorf("%w: unknown journal progress", ErrInvalid)
		}
	}
	if !sort.StringsAreSorted(journal.Completed) {
		return fmt.Errorf("%w: unstable journal progress", ErrInvalid)
	}

	return nil
}

func validIntegrationID(id string) bool {
	if !strings.HasPrefix(id, "int-") || len(id) != len("int-")+32 {
		return false
	}
	for _, character := range id[len("int-"):] {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}

	return true
}

func journalDigest(journal applyJournal) (string, error) {
	value := journal
	value.UpdatedAt = time.Time{}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}

	return stableToken(string(bytes.TrimSpace(encoded))), nil
}
