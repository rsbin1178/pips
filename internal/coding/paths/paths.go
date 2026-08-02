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
	agentDir       = ".agents"
	configFileName = "config.toml"
)

// ErrInvalid means the user configuration root cannot define a safe layout.
var ErrInvalid = errors.New("coding paths: invalid user configuration directory")

// Layout contains all user-owned P0 persistence paths. Project-scoped product
// resources remain workspace-relative and are not part of this layout.
type Layout struct {
	root                 string
	configFile           string
	workspacesFile       string
	sessionsDir          string
	plansDir             string
	teamsDir             string
	teamAggregatesDir    string
	teamContinuationsDir string
	teamResourcesDir     string
	teamControlDir       string
	teamLeasesDir        string
	teamIntegrationsDir  string
	worktreesRoot        string
	skillsDir            string
	agentSkillsDir       string
	bundlesDir           string
	mcpFile              string
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

	if filepath.Dir(abs) == abs {
		return Layout{}, fmt.Errorf("%w: filesystem root is not a product directory", ErrInvalid)
	}

	teamsDir := filepath.Join(abs, "teams")
	worktreesRoot := filepath.Join(filepath.Dir(abs), filepath.Base(abs)+"-worktrees")

	return Layout{
		root:                 abs,
		configFile:           filepath.Join(abs, configFileName),
		workspacesFile:       filepath.Join(abs, "workspaces.json"),
		sessionsDir:          filepath.Join(abs, "sessions"),
		plansDir:             filepath.Join(abs, "plans"),
		teamsDir:             teamsDir,
		teamAggregatesDir:    filepath.Join(teamsDir, "aggregates"),
		teamContinuationsDir: filepath.Join(teamsDir, "continuations"),
		teamResourcesDir:     filepath.Join(teamsDir, "resources"),
		teamControlDir:       filepath.Join(teamsDir, "control"),
		teamLeasesDir:        filepath.Join(teamsDir, "leases"),
		teamIntegrationsDir:  filepath.Join(teamsDir, "integrations"),
		worktreesRoot:        worktreesRoot,
		skillsDir:            filepath.Join(abs, "skills"),
		bundlesDir:           filepath.Join(abs, "bundles"),
		mcpFile:              filepath.Join(abs, "mcp.json"),
	}, nil
}

// Default returns the user layout selected by PIPS_HOME or ~/.pips.
func Default() (Layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, fmt.Errorf("coding paths: locate user home directory: %w", err)
	}

	if configured, ok := os.LookupEnv(HomeEnv); ok && configured != "" {
		if strings.TrimSpace(configured) == "" ||
			!filepath.IsAbs(configured) || filepath.Clean(configured) != configured {
			return Layout{}, fmt.Errorf(
				"%w: %s must be a clean absolute path",
				ErrInvalid,
				HomeEnv,
			)
		}

		layout, err := New(configured)
		if err != nil {
			return Layout{}, err
		}

		layout.agentSkillsDir = filepath.Join(home, agentDir, "skills")

		return layout, nil
	}

	layout, err := New(filepath.Join(home, productDir))
	if err != nil {
		return Layout{}, err
	}

	layout.agentSkillsDir = filepath.Join(home, agentDir, "skills")

	return layout, nil
}

// WithAgentSkillsDir returns a copy that discovers shared Agent Skills from
// directory. An empty directory disables the shared user root. This is mainly
// useful for embedders and hermetic tests; Default configures ~/.agents/skills.
func (l Layout) WithAgentSkillsDir(directory string) (Layout, error) {
	if directory == "" {
		l.agentSkillsDir = ""

		return l, nil
	}

	if strings.ContainsRune(directory, '\x00') {
		return Layout{}, fmt.Errorf("%w: Agent Skills path contains NUL", ErrInvalid)
	}

	abs, err := filepath.Abs(directory)
	if err != nil {
		return Layout{}, fmt.Errorf("%w: Agent Skills path: %w", ErrInvalid, err)
	}

	l.agentSkillsDir = abs

	return l, nil
}

// ProjectRoot returns the workspace-relative product directory.
func ProjectRoot() string { return projectDir }

// ProjectPermissionsFile returns the workspace-relative local permission path.
func ProjectPermissionsFile() string { return path.Join(projectDir, "permissions.toml") }

// ProjectMCPFile returns the workspace-relative MCP configuration path.
func ProjectMCPFile() string { return path.Join(projectDir, "mcp.json") }

// ProjectSkillsDir returns the workspace-relative project skills directory.
func ProjectSkillsDir() string { return path.Join(projectDir, "skills") }

// ProjectSkillsFile returns the workspace-relative local Skill settings path.
func ProjectSkillsFile() string { return path.Join(projectDir, "skills.toml") }

// ProjectAgentSkillsDir returns the workspace-relative shared Agent Skills
// directory.
func ProjectAgentSkillsDir() string { return path.Join(agentDir, "skills") }

// ProjectBundlesDir returns the workspace-relative project bundle directory.
func ProjectBundlesDir() string { return path.Join(projectDir, "bundles") }

// Root returns the application's user configuration directory.
func (l Layout) Root() string { return l.root }

// TempDir returns the private runtime scratch directory under the product root.
func (l Layout) TempDir() string { return filepath.Join(l.root, "tmp") }

// ConfigFile returns the user TOML configuration path.
func (l Layout) ConfigFile() string { return l.configFile }

// WorkspacesFile returns the workspace state store path.
func (l Layout) WorkspacesFile() string { return l.workspacesFile }

// SessionsDir returns the Harness session repository path.
func (l Layout) SessionsDir() string { return l.sessionsDir }

// PlansDir returns the private user-level Plan document directory.
func (l Layout) PlansDir() string { return l.plansDir }

// TeamsDir returns the private Team application data directory.
func (l Layout) TeamsDir() string { return l.teamsDir }

// TeamAggregatesDir returns the Team aggregate journal directory.
func (l Layout) TeamAggregatesDir() string { return l.teamAggregatesDir }

// TeamContinuationsDir returns the Team continuation journal directory.
func (l Layout) TeamContinuationsDir() string { return l.teamContinuationsDir }

// TeamResourcesDir returns the Team application resource journal directory.
func (l Layout) TeamResourcesDir() string { return l.teamResourcesDir }

// TeamControlDir returns the operator Team control journal directory.
func (l Layout) TeamControlDir() string { return l.teamControlDir }

// TeamLeasesDir returns the retained Team lease directory.
func (l Layout) TeamLeasesDir() string { return l.teamLeasesDir }

// TeamIntegrationsDir returns the private Integration journal directory.
func (l Layout) TeamIntegrationsDir() string { return l.teamIntegrationsDir }

// WorktreesRoot returns the sibling root reserved for Team Worktrees.
func (l Layout) WorktreesRoot() string { return l.worktreesRoot }

// SkillsDir returns the user skill directory.
func (l Layout) SkillsDir() string { return l.skillsDir }

// AgentSkillsDir returns the shared user Agent Skills directory. It is empty
// for layouts built with New unless explicitly configured.
func (l Layout) AgentSkillsDir() string { return l.agentSkillsDir }

// BundlesDir returns the user bundle directory.
func (l Layout) BundlesDir() string { return l.bundlesDir }

// MCPFile returns the user MCP configuration path.
func (l Layout) MCPFile() string { return l.mcpFile }
