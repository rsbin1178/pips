//nolint:wsl_v5 // Snapshot name validation and its bound checks stay adjacent.
package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Model snapshot file names. The model directory records exactly what was sent
// to the model and the inputs it was rendered from; it is audit material and is
// never consulted to rebuild the conversation.
const (
	// SystemPromptFileName is the rendered system prompt sent to the model.
	SystemPromptFileName = "system_prompt.txt"
	// PromptContextFileName holds the inputs the system prompt was rendered from.
	PromptContextFileName = "prompt_context.json"
	// ToolDefinitionsFileName holds the function tools sent on the latest call.
	ToolDefinitionsFileName = "tool_definitions.json"

	// maxSnapshotPromptBytes bounds a rendered system prompt.
	maxSnapshotPromptBytes = 4 << 20
	// maxSnapshotJSONBytes bounds a snapshot JSON payload.
	maxSnapshotJSONBytes = 8 << 20
	// maxSnapshotNameBytes bounds a snapshot file name.
	maxSnapshotNameBytes = 128
)

// ErrSnapshot identifies an invalid model snapshot request or payload.
var ErrSnapshot = errors.New("coding session: invalid model snapshot")

// ModelSnapshot owns the model directory of one session. It records the system
// prompt, its render inputs, and the tool definitions sent on the latest call.
type ModelSnapshot struct {
	dir string
}

// OpenModelSnapshot creates dir when needed and returns the snapshot layer for
// it. The stored path is cleaned and absolute, and dir must resolve to a real
// directory rather than a symlink.
func OpenModelSnapshot(dir string) (*ModelSnapshot, error) {
	if strings.TrimSpace(dir) == "" || strings.ContainsRune(dir, '\x00') {
		return nil, fmt.Errorf("%w: empty or invalid snapshot directory", ErrSnapshot)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: absolute snapshot directory: %w", ErrSnapshot, err)
	}

	cleaned := filepath.Clean(abs)
	if err := ensureSessionDirectory(cleaned); err != nil {
		return nil, err
	}

	return &ModelSnapshot{dir: cleaned}, nil
}

// Dir returns the absolute model snapshot directory.
func (s *ModelSnapshot) Dir() string { return s.dir }

// WriteSystemPrompt atomically publishes the rendered system prompt. Text
// larger than 4 MiB is rejected.
func (s *ModelSnapshot) WriteSystemPrompt(text string) error {
	if len(text) > maxSnapshotPromptBytes {
		return fmt.Errorf("%w: system prompt exceeds %d bytes", ErrSnapshot, maxSnapshotPromptBytes)
	}

	return writeFileAtomic(filepath.Join(s.dir, SystemPromptFileName), []byte(text), 0o600)
}

// ReadSystemPrompt returns the stored system prompt. It reports false without
// an error when the prompt was never written, and false with an error when the
// file is unreadable, larger than 4 MiB, or not a regular file.
func (s *ModelSnapshot) ReadSystemPrompt() (string, bool, error) {
	file, err := os.Open(filepath.Join(s.dir, SystemPromptFileName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("%w: open system prompt: %w", ErrSnapshot, err)
	}
	defer func() { _ = file.Close() }()

	data, err := readBoundedFile(file, maxSnapshotPromptBytes)
	if err != nil {
		return "", false, fmt.Errorf("%w: read system prompt: %w", ErrSnapshot, err)
	}

	return string(data), true, nil
}

// WriteJSON atomically publishes one JSON snapshot payload under name. Name
// must be a single path component ending in ".json" and value must encode to at
// most 8 MiB.
func (s *ModelSnapshot) WriteJSON(name string, value any) error {
	if err := validateSnapshotJSONName(name); err != nil {
		return err
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: encode %q: %w", ErrSnapshot, name, err)
	}
	if len(encoded) > maxSnapshotJSONBytes {
		return fmt.Errorf("%w: %q exceeds %d bytes", ErrSnapshot, name, maxSnapshotJSONBytes)
	}

	return writeFileAtomic(filepath.Join(s.dir, name), append(encoded, '\n'), 0o600)
}

// ReadJSON strictly decodes one JSON snapshot payload into target. It reports
// false without an error when name was never written. Unknown fields, invalid
// names, and payloads larger than 8 MiB are errors.
func (s *ModelSnapshot) ReadJSON(name string, target any) (bool, error) {
	if err := validateSnapshotJSONName(name); err != nil {
		return false, err
	}

	file, err := os.Open(filepath.Join(s.dir, name)) //nolint:gosec // caller-owned snapshot path
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("%w: open %q: %w", ErrSnapshot, name, err)
	}
	defer func() { _ = file.Close() }()

	data, err := readBoundedFile(file, maxSnapshotJSONBytes)
	if err != nil {
		return false, fmt.Errorf("%w: read %q: %w", ErrSnapshot, name, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false, fmt.Errorf("%w: decode %q: %w", ErrSnapshot, name, err)
	}

	return true, nil
}

// validateSnapshotJSONName rejects a name that is not one safe path component
// ending in ".json".
func validateSnapshotJSONName(name string) error {
	if err := validateSnapshotComponent(name); err != nil {
		return err
	}
	if !strings.HasSuffix(name, ".json") {
		return fmt.Errorf("%w: %q is not a .json name", ErrSnapshot, name)
	}

	return nil
}

// validateSnapshotComponent rejects a name that is not a single safe path
// component.
func validateSnapshotComponent(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("%w: invalid snapshot name %q", ErrSnapshot, name)
	}
	if len(name) > maxSnapshotNameBytes {
		return fmt.Errorf("%w: snapshot name exceeds %d bytes", ErrSnapshot, maxSnapshotNameBytes)
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("%w: snapshot name %q is not a single path component", ErrSnapshot, name)
	}

	return nil
}
