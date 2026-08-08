// Package agentprofile loads immutable, declarative Coding subagent profiles.
// It owns discovery and validation only; Runtime compiles profiles against the
// current capability and approval policy before any profile can execute.
package agentprofile

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
)

var (
	// ErrInvalid reports malformed profile options or profile data.
	ErrInvalid = errors.New("coding agent profile: invalid")
	// ErrLimitExceeded reports bounded profile discovery or content exhaustion.
	ErrLimitExceeded = errors.New("coding agent profile: limit exceeded")
	// ErrInsecure reports an unsafe user-owned profile root.
	ErrInsecure = errors.New("coding agent profile: insecure root")
)

const (
	// SchemaV1Alpha1 is the only custom profile frontmatter schema accepted by
	// this release.
	SchemaV1Alpha1 = "pips.agent/v1alpha1"
	modelInherit   = "inherit"

	maximumDiagnosticMessageBytes = 512
)

// Kind identifies whether a profile is supplied by Pips or declarative input.
type Kind string

const (
	// KindBuiltin identifies a program-owned compatibility profile.
	KindBuiltin Kind = "builtin"
	// KindCustom identifies a profile loaded from a user or trusted project root.
	KindCustom Kind = "custom"
	// KindEphemeral identifies a validated one-shot profile. It is reserved for
	// the explicit one-shot path and is not discovered from roots.
	KindEphemeral Kind = "ephemeral"
)

// Scope identifies the safe source boundary that supplied a profile.
type Scope string

const (
	// ScopeBuiltin is a program-owned compatibility profile.
	ScopeBuiltin Scope = "builtin"
	// ScopeUserShared is the shared ~/.agents/agents root.
	ScopeUserShared Scope = "user:shared"
	// ScopeUserPips is the private ~/.pips/agents root.
	ScopeUserPips Scope = "user:pips"
	// ScopeProjectShared is the trusted .agents/agents root.
	ScopeProjectShared Scope = "project:shared"
	// ScopeProjectPips is the trusted .pips/agents root.
	ScopeProjectPips Scope = "project:pips"
	// ScopeEphemeral identifies an explicitly submitted one-shot definition.
	// It is never discovered from disk or written by profile parsing.
	ScopeEphemeral Scope = "ephemeral"
)

// Delivery selects a profile's eligible execution delivery mode.
type Delivery string

const (
	// DeliveryForeground waits for the child terminal result.
	DeliveryForeground Delivery = "foreground"
	// DeliveryBackground returns a durable child reference immediately.
	DeliveryBackground Delivery = "background"
)

// Audience identifies the caller whose visible profiles are requested.
type Audience string

const (
	// AudienceUser identifies explicit user-facing entry points.
	AudienceUser Audience = "user"
	// AudienceModel identifies the parent model's delegation Tool surface.
	AudienceModel Audience = "model"
)

// Visibility controls which caller can discover a profile. A profile may still
// be unavailable at dispatch when its current runtime requirements are unmet.
type Visibility struct {
	User  bool
	Model bool
}

// VisibleFor reports whether the requested caller may discover a profile.
func (v Visibility) VisibleFor(audience Audience) bool {
	switch audience {
	case AudienceUser:
		return v.User
	case AudienceModel:
		return v.Model
	default:
		return false
	}
}

// ExecutionLimits are requested upper bounds. Zero means inherit the Runtime
// ceiling; profile compilation may only narrow that ceiling.
type ExecutionLimits struct {
	MaxTurns     int
	MaxToolCalls int
	MaxDuration  time.Duration
}

// SelectorKind identifies the exact catalog dimension selected by a profile.
type SelectorKind string

const (
	// SelectorTool selects one exact Tool wire name.
	SelectorTool SelectorKind = "tool"
	// SelectorSource selects Tools from one catalog source identity.
	SelectorSource SelectorKind = "source"
	// SelectorTag selects Tools carrying one Runtime-owned catalog tag.
	SelectorTag SelectorKind = "tag"
)

// Selector is a parsed profile capability request. It is not an authority
// grant: Runtime intersects it with its active delegable catalog.
type Selector struct {
	Kind  SelectorKind
	Value string
}

// String returns the canonical selector spelling stored in a profile snapshot.
func (s Selector) String() string {
	if s.Kind == "" || s.Value == "" {
		return ""
	}

	return string(s.Kind) + ":" + s.Value
}

// ToolSelection constrains the Runtime-owned delegable Tool catalog.
type ToolSelection struct {
	Allow      []Selector
	Require    []Selector
	ToolSearch bool
}

// SkillSelection constrains the immutable Skill snapshot selected at dispatch.
type SkillSelection struct {
	Allow   []string
	Preload []string
}

// OutputFormat identifies the local final-result contract for a custom profile.
type OutputFormat string

const (
	// OutputText accepts a bounded UTF-8 final response.
	OutputText OutputFormat = "text"
	// OutputJSONSchema validates a bounded JSON final response locally.
	OutputJSONSchema OutputFormat = "json_schema"
)

// OutputContract is the declarative final-result contract. Schema contains
// canonical JSON bytes rather than YAML nodes so callers never retain mutable
// parser state.
type OutputContract struct {
	Format OutputFormat
	Schema json.RawMessage
}

// Definition is one fully parsed immutable-by-convention profile snapshot.
// Source never contains an absolute local filesystem path.
type Definition struct {
	ID           string
	Kind         Kind
	Scope        Scope
	Source       string
	Digest       string
	Schema       string
	Name         string
	Description  string
	Instructions string
	Model        string
	Visibility   Visibility
	Delivery     []Delivery
	Limits       ExecutionLimits
	Tools        ToolSelection
	Skills       SkillSelection
	Output       OutputContract
}

// Clone returns a detached definition snapshot.
func (d Definition) Clone() Definition {
	cloned := d
	cloned.Delivery = slices.Clone(d.Delivery)
	cloned.Tools.Allow = slices.Clone(d.Tools.Allow)
	cloned.Tools.Require = slices.Clone(d.Tools.Require)
	cloned.Skills.Allow = slices.Clone(d.Skills.Allow)
	cloned.Skills.Preload = slices.Clone(d.Skills.Preload)
	cloned.Output.Schema = slices.Clone(d.Output.Schema)

	return cloned
}

// EntryStatus describes whether a discovered profile is active or retained for
// diagnostics only.
type EntryStatus string

const (
	// EntryAvailable is the winning active profile for its canonical ID.
	EntryAvailable EntryStatus = "available"
	// EntrySuppressed is a valid lower-precedence profile.
	EntrySuppressed EntryStatus = "suppressed"
	// EntryInvalid is an invalid or reserved-ID profile that cannot dispatch.
	EntryInvalid EntryStatus = "invalid"
)

// Diagnostic is a bounded, content-free loading decision. Source, Winner, and
// Suppressed use safe provenance strings rather than absolute filesystem paths.
type Diagnostic struct {
	Code       string
	Source     string
	Winner     string
	Suppressed string
	Message    string
}

// Entry preserves valid suppressed and invalid discovery records for Library
// diagnostics without exposing profile body content.
type Entry struct {
	ID           string
	Kind         Kind
	Scope        Scope
	Source       string
	Digest       string
	Status       EntryStatus
	SuppressedBy string
	Definition   Definition
	Diagnostics  []Diagnostic
}

// Clone returns a detached entry snapshot.
func (e Entry) Clone() Entry {
	cloned := e
	cloned.Definition = e.Definition.Clone()
	cloned.Diagnostics = slices.Clone(e.Diagnostics)

	return cloned
}

// Registry is an immutable loading snapshot. It intentionally has no model,
// Tool, or credential dependency, so parsing a profile cannot grant authority.
type Registry struct {
	entries     []Entry
	definitions []Definition
	diagnostics []Diagnostic
}

// Clone returns a fully detached registry snapshot.
//
//nolint:wsl_v5 // Each defensive copy stays beside the immutable field it protects.
func (r Registry) Clone() Registry {
	cloned := Registry{
		entries:     make([]Entry, len(r.entries)),
		definitions: make([]Definition, len(r.definitions)),
		diagnostics: slices.Clone(r.diagnostics),
	}
	for index := range r.entries {
		cloned.entries[index] = r.entries[index].Clone()
	}
	for index := range r.definitions {
		cloned.definitions[index] = r.definitions[index].Clone()
	}

	return cloned
}

// Entries returns all known discovery records in deterministic order.
func (r Registry) Entries() []Entry {
	entries := make([]Entry, len(r.entries))
	for index := range r.entries {
		entries[index] = r.entries[index].Clone()
	}

	return entries
}

// List returns active definitions in canonical ID order.
func (r Registry) List() []Definition {
	definitions := make([]Definition, len(r.definitions))
	for index := range r.definitions {
		definitions[index] = r.definitions[index].Clone()
	}

	return definitions
}

// Lookup returns the active profile for id.
func (r Registry) Lookup(id string) (Definition, bool) {
	for _, definition := range r.definitions {
		if definition.ID == id {
			return definition.Clone(), true
		}
	}

	return Definition{}, false
}

// VisibleFor returns active profiles visible to the selected caller.
func (r Registry) VisibleFor(audience Audience) []Definition {
	definitions := make([]Definition, 0, len(r.definitions))
	for _, definition := range r.definitions {
		if definition.Visibility.VisibleFor(audience) {
			definitions = append(definitions, definition.Clone())
		}
	}

	return definitions
}

// Diagnostics returns deterministic loading diagnostics.
func (r Registry) Diagnostics() []Diagnostic { return slices.Clone(r.diagnostics) }
