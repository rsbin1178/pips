package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const applicationDir = "pips"

// ErrInvalid means the user configuration root cannot define a safe layout.
var ErrInvalid = errors.New("coding paths: invalid user configuration directory")

// Layout contains all user-owned P0 persistence paths. Project configuration
// remains workspace-relative and is not part of this layout.
type Layout struct {
	root        string
	configFile  string
	trustFile   string
	sessionsDir string
}

// New returns the application layout below userConfigDir.
func New(userConfigDir string) (Layout, error) {
	if strings.TrimSpace(userConfigDir) == "" {
		return Layout{}, fmt.Errorf("%w: empty path", ErrInvalid)
	}

	abs, err := filepath.Abs(userConfigDir)
	if err != nil {
		return Layout{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	root := filepath.Join(abs, applicationDir)

	return Layout{
		root:        root,
		configFile:  filepath.Join(root, "config.toml"),
		trustFile:   filepath.Join(root, "trust.json"),
		sessionsDir: filepath.Join(root, "sessions"),
	}, nil
}

// Default returns the layout below the operating system user configuration
// directory.
func Default() (Layout, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Layout{}, fmt.Errorf("coding paths: locate user configuration directory: %w", err)
	}

	return New(dir)
}

// Root returns the application's user configuration directory.
func (l Layout) Root() string { return l.root }

// ConfigFile returns the user TOML configuration path.
func (l Layout) ConfigFile() string { return l.configFile }

// TrustFile returns the workspace trust-store path.
func (l Layout) TrustFile() string { return l.trustFile }

// SessionsDir returns the Harness session repository path.
func (l Layout) SessionsDir() string { return l.sessionsDir }
