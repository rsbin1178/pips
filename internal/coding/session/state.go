//nolint:wsl_v5 // Bounds checks and their storage operations stay adjacent.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// State sidecar bounds. Every artifact is small and rebuildable, so a reader
// never trusts its size or shape.
const (
	maxStateNameBytes      = 128
	maxStateJSONBytes      = 1 << 20
	maxStateLineBytes      = 1 << 20
	defaultStateJSONLLimit = 1000
	maxStateJSONLLimit     = 100_000
)

// ErrState identifies an invalid state sidecar request or payload.
var ErrState = errors.New("coding session: invalid state sidecar")

// StateStore owns the derived, rebuildable state directory of one session. It
// reads and writes the small sidecars there (plan mode, usage, signals, rewind
// points) and is never authoritative for conversation content.
type StateStore struct {
	dir string
}

// OpenStateStore opens the state directory of one session, creating it when
// absent. The directory is private and must be a real directory rather than a
// symlink.
func OpenStateStore(dir string) (*StateStore, error) {
	if strings.TrimSpace(dir) == "" || strings.ContainsRune(dir, '\x00') {
		return nil, fmt.Errorf("%w: empty or invalid state directory", ErrState)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: absolute state directory: %w", ErrState, err)
	}

	abs = filepath.Clean(abs)
	if err := ensureSessionDirectory(abs); err != nil {
		return nil, err
	}

	return &StateStore{dir: abs}, nil
}

// Dir returns the absolute state directory.
func (s *StateStore) Dir() string { return s.dir }

// ReadJSON decodes one state sidecar into target. A missing file reports
// (false, nil); a present but unreadable or undecodable file reports an error
// wrapping [ErrState].
func (s *StateStore) ReadJSON(name string, target any) (bool, error) {
	if err := validateStateName(name); err != nil {
		return false, err
	}
	if target == nil {
		return false, fmt.Errorf("%w: nil JSON target for %q", ErrState, name)
	}

	file, err := os.Open(s.path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("coding session: open state file %q: %w", name, err)
	}
	defer func() { _ = file.Close() }()

	data, err := readBoundedFile(file, maxStateJSONBytes)
	if err != nil {
		return false, fmt.Errorf("%w: read state file %q: %w", ErrState, name, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false, fmt.Errorf("%w: decode state file %q: %w", ErrState, name, err)
	}

	return true, nil
}

// WriteJSON atomically publishes value as one state sidecar.
func (s *StateStore) WriteJSON(name string, value any) error {
	if err := validateStateName(name); err != nil {
		return err
	}
	if value == nil {
		return fmt.Errorf("%w: nil JSON value for %q", ErrState, name)
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: encode state file %q: %w", ErrState, name, err)
	}
	if len(encoded) > maxStateJSONBytes {
		return fmt.Errorf("%w: state file %q exceeds %d bytes", ErrState, name, maxStateJSONBytes)
	}

	return writeFileAtomic(s.path(name), append(encoded, '\n'), 0o600)
}

// AppendJSONL appends one compact JSON value per line. An empty values slice
// writes nothing and creates no file.
func (s *StateStore) AppendJSONL(name string, values ...any) error {
	if err := validateStateName(name); err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}

	var buffer bytes.Buffer
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("%w: encode state line in %q: %w", ErrState, name, err)
		}
		if len(encoded) > maxStateLineBytes {
			return fmt.Errorf("%w: state line in %q exceeds %d bytes", ErrState, name, maxStateLineBytes)
		}

		buffer.Write(encoded)
		buffer.WriteByte('\n')
	}

	file, err := os.OpenFile(s.path(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("coding session: open state file %q: %w", name, err)
	}
	if _, err := file.Write(buffer.Bytes()); err != nil {
		return errors.Join(fmt.Errorf("coding session: append state file %q: %w", name, err), file.Close())
	}

	return file.Close()
}

// ReadJSONL returns up to limit raw JSON lines oldest first. A limit <= 0 uses
// the default; a missing file reports (nil, nil). A trailing partial line is
// dropped, while a malformed interior line wraps [ErrState].
func (s *StateStore) ReadJSONL(name string, limit int) ([][]byte, error) {
	if err := validateStateName(name); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultStateJSONLLimit
	}
	if limit > maxStateJSONLLimit {
		limit = maxStateJSONLLimit
	}

	file, err := os.Open(s.path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("coding session: open state file %q: %w", name, err)
	}
	defer func() { _ = file.Close() }()

	return readStateLines(file, name, limit)
}

// readStateLines returns up to limit raw JSON lines from an open sidecar,
// dropping a trailing partial line and rejecting a malformed interior line.
func readStateLines(file *os.File, name string, limit int) ([][]byte, error) {
	reader := bufio.NewReaderSize(file, 64<<10)
	lines := make([][]byte, 0, min(limit, 64))
	line := make([]byte, 0, 512)

	for len(lines) < limit {
		chunk, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			line = append(line, chunk...)
			if len(line) > maxStateLineBytes {
				return nil, fmt.Errorf("%w: state line in %q exceeds %d bytes", ErrState, name, maxStateLineBytes)
			}

			continue
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("coding session: read state file %q: %w", name, readErr)
		}

		line = append(line, chunk...)
		complete := len(line) > 0 && line[len(line)-1] == '\n'
		trimmed := bytes.TrimRight(line, "\r\n")

		switch {
		case len(trimmed) == 0:
		case !complete:
			// A partial final line is a torn tail from an interrupted append.
		default:
			if !json.Valid(trimmed) {
				return nil, fmt.Errorf("%w: malformed state line in %q", ErrState, name)
			}

			lines = append(lines, bytes.Clone(trimmed))
		}

		line = line[:0]
		if errors.Is(readErr, io.EOF) || !complete {
			break
		}
	}

	return lines, nil
}

// Remove deletes one state sidecar. A missing file is not an error.
func (s *StateStore) Remove(name string) error {
	if err := validateStateName(name); err != nil {
		return err
	}
	if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("coding session: remove state file %q: %w", name, err)
	}

	return nil
}

func (s *StateStore) path(name string) string {
	return filepath.Join(s.dir, name)
}

// validateStateName enforces that name is exactly one safe path component.
func validateStateName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > maxStateNameBytes {
		return fmt.Errorf("%w: invalid state name %q", ErrState, name)
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("%w: invalid state name %q", ErrState, name)
	}

	return nil
}
