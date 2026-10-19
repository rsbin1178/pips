// Package subagentprofile loads the declarative role and persona presets that
// parameterize a delegated child. It owns discovery and validation only; the
// Runtime resolves a preset against its captured model and tool catalogs
// before any child runs, so a preset can never grant authority by itself.
package subagentprofile

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

var (
	// ErrInvalid reports a malformed preset declaration or option set.
	ErrInvalid = errors.New("coding subagent profile: invalid")
	// ErrLimitExceeded reports bounded preset discovery or content exhaustion.
	ErrLimitExceeded = errors.New("coding subagent profile: limit exceeded")
	// ErrInsecure reports an unsafe user-owned preset root.
	ErrInsecure = errors.New("coding subagent profile: insecure root")
)

// Limits bounds preset discovery and content.
type Limits struct {
	MaxEntries    int
	MaxPresets    int
	MaxFileBytes  int64
	MaxTotalBytes int64
	// MaxPromptBytes bounds one resolved role prompt or persona instruction
	// file.
	MaxPromptBytes int64
	MaxNameBytes   int
}

// DefaultLimits returns the production preset discovery bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxEntries:     1_024,
		MaxPresets:     256,
		MaxFileBytes:   64 << 10,
		MaxTotalBytes:  2 << 20,
		MaxPromptBytes: 64 << 10,
		MaxNameBytes:   64,
	}
}

func validateLimits(limits Limits) error {
	if limits.MaxEntries <= 0 || limits.MaxPresets <= 0 || limits.MaxFileBytes <= 0 ||
		limits.MaxTotalBytes <= 0 || limits.MaxPromptBytes <= 0 || limits.MaxNameBytes <= 0 {
		return fmt.Errorf("%w: preset limits must be positive", ErrInvalid)
	}

	return nil
}

// Options select the user root, the optional trusted project root, and the
// inline declarations that take precedence over both.
type Options struct {
	Paths          paths.Layout
	Tree           *workspace.Tree
	ProjectTrusted bool
	InlineRoles    map[string]config.SubagentRole
	InlinePersonas map[string]config.SubagentPersona
	Limits         Limits
}

// Diagnostic is a bounded, content-free discovery decision.
type Diagnostic struct {
	Code       string
	Source     string
	Winner     string
	Suppressed string
	Message    string
}

// Registry is an immutable snapshot of the resolved presets. It has no model,
// Tool, or credential dependency, so holding it cannot grant authority.
type Registry struct {
	roles       map[string]config.SubagentRole
	personas    map[string]config.SubagentPersona
	diagnostics []Diagnostic
}

// Role returns the resolved role for name.
func (r Registry) Role(name string) (config.SubagentRole, bool) {
	role, ok := r.roles[name]
	if !ok {
		return config.SubagentRole{}, false
	}

	return cloneRole(role), true
}

// Persona returns the resolved persona for name.
func (r Registry) Persona(name string) (config.SubagentPersona, bool) {
	persona, ok := r.personas[name]
	if !ok {
		return config.SubagentPersona{}, false
	}

	return clonePersona(persona), true
}

// Clone returns a detached registry snapshot.
func (r Registry) Clone() Registry {
	cloned := Registry{
		roles:       make(map[string]config.SubagentRole, len(r.roles)),
		personas:    make(map[string]config.SubagentPersona, len(r.personas)),
		diagnostics: append([]Diagnostic(nil), r.diagnostics...),
	}
	for name, role := range r.roles {
		cloned.roles[name] = cloneRole(role)
	}

	for name, persona := range r.personas {
		cloned.personas[name] = clonePersona(persona)
	}

	return cloned
}

// Diagnostics returns deterministic discovery diagnostics.
func (r Registry) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), r.diagnostics...)
}

func cloneRole(role config.SubagentRole) config.SubagentRole { return role }

func clonePersona(persona config.SubagentPersona) config.SubagentPersona {
	persona.Inputs = append([]config.SubagentIOField(nil), persona.Inputs...)
	persona.Outputs = append([]config.SubagentIOField(nil), persona.Outputs...)

	return persona
}

// validPresetName reports whether a file stem or inline key can name a preset.
// A preset is selected by an agent type name, so it uses the same alphabet.
func validPresetName(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.HasPrefix(value, "-") ||
		strings.HasSuffix(value, "-") || strings.Contains(value, "--") {
		return false
	}

	for _, current := range value {
		if current == '-' || current >= 'a' && current <= 'z' || current >= '0' && current <= '9' {
			continue
		}

		return false
	}

	return true
}

// validRelativeFile reports whether a configured prompt or instruction path is
// a clean workspace-relative path that cannot escape its root.
func validRelativeFile(value string) bool {
	if value == "" || len(value) > 4<<10 || !utf8.ValidString(value) ||
		strings.ContainsRune(value, '\x00') || filepath.IsAbs(value) ||
		strings.HasPrefix(value, "~") {
		return false
	}

	cleaned := filepath.ToSlash(filepath.Clean(value))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}

	return cleaned == filepath.ToSlash(value)
}
