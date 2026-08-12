// Package agentplugin loads portable Agent Plugins 1.0.0 packages.
package agentplugin

import (
	"errors"
	"slices"

	"github.com/rsbin1178/pips/agent/extension"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const (
	// ManifestSchema is the canonical Agent Plugins 1.0.0 manifest schema ID.
	ManifestSchema = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	// MCPSchema is the canonical Agent Plugins 1.0.0 MCP schema ID.
	MCPSchema = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

var (
	// ErrInvalid reports a package or component that violates Agent Plugins.
	ErrInvalid = errors.New("agent plugin: invalid")
	// ErrLimitExceeded reports bounded discovery or content exhaustion.
	ErrLimitExceeded = errors.New("agent plugin: limit exceeded")
)

// Limits bound local package discovery and reads.
type Limits struct {
	MaxPlugins            int
	MaxManifestBytes      int64
	MaxMCPBytes           int64
	MaxMCPServers         int
	MaxSkills             int
	MaxSkillBytes         int64
	MaxResourcesPerSkill  int
	MaxResourceBytes      int64
	MaxTotalResourceBytes int64
}

// DefaultLimits returns conservative Agent Plugin loading bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxPlugins: 64, MaxManifestBytes: 1 << 20, MaxMCPBytes: 1 << 20,
		MaxMCPServers: 64, MaxSkills: 256, MaxSkillBytes: 1 << 20,
		MaxResourcesPerSkill: 128, MaxResourceBytes: 1 << 20,
		MaxTotalResourceBytes: 16 << 20,
	}
}

// Options select user and optional trusted-project plugin roots.
type Options struct {
	Paths          paths.Layout
	Tree           *workspace.Tree
	ProjectTrusted bool
	Limits         Limits
}

// Author is optional portable plugin author metadata.
type Author struct {
	Name  string
	Email string
	URL   string
}

// Manifest is the portable metadata from plugin.json.
type Manifest struct {
	Name        string
	Version     string
	Description string
	Author      *Author
	Homepage    string
	Repository  string
	License     string
	Keywords    []string
}

// Package is one accepted portable plugin package.
type Package struct {
	Manifest    Manifest
	Provenance  string
	Root        string
	Data        string
	SkillCount  int
	ServerCount int
	instance    string
}

// Diagnostic describes a non-fatal package or component decision.
type Diagnostic struct {
	Plugin    string `json:"plugin"`
	Component string `json:"component"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// Result is one fully decoded Agent Plugin generation.
type Result struct {
	packages    []Package
	skills      []extension.SkillEntry
	definitions codingmcp.Definitions
	diagnostics []Diagnostic
}

// Packages returns accepted portable packages in deterministic order.
func (r Result) Packages() []Package { return slices.Clone(r.packages) }

// SkillEntries returns accepted Agent Skills with plugin provenance.
func (r Result) SkillEntries() []extension.SkillEntry { return slices.Clone(r.skills) }

// MCPDefinitions returns validated portable MCP server definitions.
func (r Result) MCPDefinitions() codingmcp.Definitions { return r.definitions }

// Diagnostics returns deterministic load diagnostics.
func (r Result) Diagnostics() []Diagnostic { return slices.Clone(r.diagnostics) }
