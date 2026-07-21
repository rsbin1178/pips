package paths

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// HomeEnv overrides the default ~/.pips product root.
	HomeEnv        = "PIPS_HOME"
	productDir     = ".pips"
	projectDir     = ".pips"
	configFileName = "config.toml"
)

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

// New returns an application layout rooted exactly at root.
func New(root string) (Layout, error) {
	if strings.TrimSpace(root) == "" {
		return Layout{}, fmt.Errorf("%w: empty path", ErrInvalid)
	}

	if strings.ContainsRune(root, '\x00') {
		return Layout{}, fmt.Errorf("%w: path contains NUL", ErrInvalid)
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return Layout{
		root:        abs,
		configFile:  filepath.Join(abs, configFileName),
		trustFile:   filepath.Join(abs, "trust.json"),
		sessionsDir: filepath.Join(abs, "sessions"),
	}, nil
}

// Default returns the user layout selected by PIPS_HOME or ~/.pips.
func Default() (Layout, error) {
	if configured, ok := os.LookupEnv(HomeEnv); ok && configured != "" {
		if strings.TrimSpace(configured) == "" ||
			!filepath.IsAbs(configured) || filepath.Clean(configured) != configured {
			return Layout{}, fmt.Errorf(
				"%w: %s must be a clean absolute path",
				ErrInvalid,
				HomeEnv,
			)
		}

		return New(configured)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, fmt.Errorf("coding paths: locate user home directory: %w", err)
	}

	return New(filepath.Join(home, productDir))
}

// ProjectRoot returns the workspace-relative product directory.
func ProjectRoot() string { return projectDir }

// ProjectConfigFile returns the workspace-relative project configuration path.
func ProjectConfigFile() string { return path.Join(projectDir, configFileName) }

// ProjectPermissionsFile returns the workspace-relative local permission path.
func ProjectPermissionsFile() string { return path.Join(projectDir, "permissions.toml") }

// ProjectMCPFile returns the workspace-relative MCP configuration path.
func ProjectMCPFile() string { return path.Join(projectDir, "mcp.json") }

// ProjectSkillsDir returns the workspace-relative project skills directory.
func ProjectSkillsDir() string { return path.Join(projectDir, "skills") }

// ProjectBundlesDir returns the workspace-relative project bundle directory.
func ProjectBundlesDir() string { return path.Join(projectDir, "bundles") }

// Root returns the application's user configuration directory.
func (l Layout) Root() string { return l.root }

// ConfigFile returns the user TOML configuration path.
func (l Layout) ConfigFile() string { return l.configFile }

// TrustFile returns the workspace trust-store path.
func (l Layout) TrustFile() string { return l.trustFile }

// SessionsDir returns the Harness session repository path.
func (l Layout) SessionsDir() string { return l.sessionsDir }
