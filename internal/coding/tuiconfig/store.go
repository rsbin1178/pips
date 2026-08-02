// Package tuiconfig persists presentation-only TUI preferences separately
// from runtime capability configuration.
package tuiconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/rsbin/pips/internal/coding/statusline"
	"github.com/rsbin/pips/internal/jsonx"
)

const (
	Schema       = "pips.tui/v1alpha1"
	maximumBytes = 64 << 10
)

// Settings is the complete presentation preference snapshot.
type Settings struct {
	StatusLine []statusline.Item
}

// Clone returns detached settings.
func (settings Settings) Clone() Settings {
	settings.StatusLine = slices.Clone(settings.StatusLine)

	return settings
}

// Store owns one private JSON preference file.
type Store struct {
	path string
}

// NewStore returns a preference store for path.
func NewStore(path string) *Store { return &Store{path: path} }

type file struct {
	Schema     string `json:"schema"`
	StatusLine struct {
		Items []statusline.Item `json:"items"`
	} `json:"status_line"`
}

// Load returns defaults when the preference file does not exist.
func (store *Store) Load() (Settings, error) {
	input, err := os.Open(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{StatusLine: statusline.Default()}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("coding TUI config: open: %w", err)
	}
	defer input.Close()
	if err := validatePrivateFile(store.path); err != nil {
		return Settings{}, err
	}
	data, err := io.ReadAll(io.LimitReader(input, maximumBytes+1))
	if err != nil {
		return Settings{}, fmt.Errorf("coding TUI config: read: %w", err)
	}
	if len(data) > maximumBytes {
		return Settings{}, errors.New("coding TUI config: file is too large")
	}

	var decoded file
	if err := jsonx.Decode(data, &decoded); err != nil {
		return Settings{}, fmt.Errorf("coding TUI config: decode: %w", err)
	}
	if decoded.Schema != Schema {
		return Settings{}, fmt.Errorf("coding TUI config: unsupported schema %q", decoded.Schema)
	}
	if err := statusline.Validate(decoded.StatusLine.Items); err != nil {
		return Settings{}, fmt.Errorf("coding TUI config: %w", err)
	}

	return Settings{StatusLine: slices.Clone(decoded.StatusLine.Items)}, nil
}

// Save atomically replaces the complete preference snapshot.
func (store *Store) Save(settings Settings) (returnErr error) {
	if err := statusline.Validate(settings.StatusLine); err != nil {
		return fmt.Errorf("coding TUI config: %w", err)
	}

	directory := filepath.Dir(store.path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}

	encoded := file{Schema: Schema}
	encoded.StatusLine.Items = slices.Clone(settings.StatusLine)
	data, err := json.MarshalIndent(encoded, "", "  ")
	if err != nil {
		return fmt.Errorf("coding TUI config: encode: %w", err)
	}
	data = append(data, '\n')

	temporary, err := os.CreateTemp(directory, ".tui-*.json")
	if err != nil {
		return fmt.Errorf("coding TUI config: create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, temporary.Close())
		}
		if returnErr != nil {
			returnErr = errors.Join(returnErr, os.Remove(temporaryPath))
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("coding TUI config: secure temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("coding TUI config: write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("coding TUI config: sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("coding TUI config: close temporary file: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return fmt.Errorf("coding TUI config: replace: %w", err)
	}

	return nil
}

func ensurePrivateDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("coding TUI config: create directory: %w", err)
	}

	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("coding TUI config: inspect directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("coding TUI config: directory %q must have mode 0700", directory)
	}

	return nil
}

func validatePrivateFile(path string) error {
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("coding TUI config: inspect file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("coding TUI config: file %q must have mode 0600", path)
	}

	return nil
}
