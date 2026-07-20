package bundle

import (
	"maps"
	"slices"

	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
)

// SchemaV1Alpha1 is the P0 Bundle manifest schema.
const SchemaV1Alpha1 = "pips.bundle/v1alpha1"

// DefaultManifestPath is the conventional manifest path within a bundle.
const DefaultManifestPath = "pips-bundle.json"

// Scope identifies who selected a Bundle source. Scope does not come from the
// bundle itself, so a manifest cannot promote its own trust level.
type Scope string

// Supported Bundle scopes.
const (
	ScopeManaged   Scope = "managed"
	ScopeUser      Scope = "user"
	ScopeProject   Scope = "project"
	ScopeTemporary Scope = "temporary"
)

// TrustDecision is the application's explicit trust state for a Bundle.
type TrustDecision uint8

// Bundle trust states.
const (
	TrustUnspecified TrustDecision = iota
	TrustDenied
	TrustApproved
)

// ComponentFilter narrows one component kind. A nil Include means all
// declared values; a non-nil empty Include means none. Exclude is then applied
// to the included set. Every filter value must exist in the manifest.
type ComponentFilter struct {
	Include []string
	Exclude []string
}

// Filters independently narrow each manifest component kind. Extensions are
// filtered by registered ID; Skills, Prompts, and Assets are filtered by their
// declared path.
type Filters struct {
	Extensions ComponentFilter
	Skills     ComponentFilter
	Prompts    ComponentFilter
	Assets     ComponentFilter
}

// Settings are application-owned selection metadata for one Load call.
type Settings struct {
	Scope   Scope
	Trust   TrustDecision
	Filters Filters
}

// Manifest is the strict JSON contract for one local bundle.
type Manifest struct {
	Schema      string                 `json:"schema"`
	ID          string                 `json:"id"`
	Version     string                 `json:"version"`
	Description string                 `json:"description,omitempty"`
	Requires    []extension.Capability `json:"requires,omitempty"`
	Optional    []extension.Capability `json:"optional,omitempty"`
	Extensions  []string               `json:"extensions,omitempty"`
	Skills      []string               `json:"skills,omitempty"`
	Prompts     []string               `json:"prompts,omitempty"`
	Assets      []AssetSpec            `json:"assets,omitempty"`
}

// AssetSpec declares one opaque typed asset file.
type AssetSpec struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	MediaType string `json:"media_type,omitempty"`
}

// Limits bound all reads performed for one Bundle. Resource bytes include
// Skill manifests, prompt templates, and asset data, but not the separately
// bounded manifest.
type Limits struct {
	MaxManifestBytes int64
	MaxResourceBytes int64
	MaxTotalBytes    int64
	MaxResources     int
}

// DefaultLimits returns conservative P0 local-bundle limits.
func DefaultLimits() Limits {
	return Limits{
		MaxManifestBytes: 256 << 10,
		MaxResourceBytes: 1 << 20,
		MaxTotalBytes:    8 << 20,
		MaxResources:     128,
	}
}

// Diagnostic is a non-fatal, path-scoped load condition.
type Diagnostic struct {
	Component string
	Path      string
	Message   string
}

// Bundle is one fully decoded and validated manifest selection. Its methods return
// defensive copies and it contains no open files or running resources.
type Bundle struct {
	manifest     Manifest
	scope        Scope
	extensionIDs []string
	skills       []harness.Skill
	prompts      []harness.PromptTemplate
	assets       []extension.Asset
	diagnostics  []Diagnostic
}

// Manifest returns a defensive copy of the bundle manifest.
func (b *Bundle) Manifest() Manifest {
	if b == nil {
		return Manifest{}
	}

	return cloneManifest(b.manifest)
}

// Scope returns the application-selected bundle scope.
func (b *Bundle) Scope() Scope {
	if b == nil {
		return ""
	}

	return b.scope
}

// ExtensionIDs returns selected registered Extension IDs in manifest order.
func (b *Bundle) ExtensionIDs() []string {
	if b == nil {
		return nil
	}

	return slices.Clone(b.extensionIDs)
}

// Skills returns selected Agent Skills with defensive metadata copies.
func (b *Bundle) Skills() []harness.Skill {
	if b == nil {
		return nil
	}

	values := make([]harness.Skill, len(b.skills))
	for i, skill := range b.skills {
		skill.Metadata = maps.Clone(skill.Metadata)
		skill.AllowedTools = slices.Clone(skill.AllowedTools)
		values[i] = skill
	}

	return values
}

// Prompts returns selected prompt templates.
func (b *Bundle) Prompts() []harness.PromptTemplate {
	if b == nil {
		return nil
	}

	return slices.Clone(b.prompts)
}

// Assets returns selected opaque assets with defensive data copies.
func (b *Bundle) Assets() []extension.Asset {
	if b == nil {
		return nil
	}

	values := make([]extension.Asset, len(b.assets))
	for i, asset := range b.assets {
		asset.Data = slices.Clone(asset.Data)
		values[i] = asset
	}

	return values
}

// Diagnostics returns non-fatal load diagnostics.
func (b *Bundle) Diagnostics() []Diagnostic {
	if b == nil {
		return nil
	}

	return slices.Clone(b.diagnostics)
}

func cloneManifest(manifest Manifest) Manifest {
	manifest.Requires = slices.Clone(manifest.Requires)
	manifest.Optional = slices.Clone(manifest.Optional)
	manifest.Extensions = slices.Clone(manifest.Extensions)
	manifest.Skills = slices.Clone(manifest.Skills)
	manifest.Prompts = slices.Clone(manifest.Prompts)
	manifest.Assets = slices.Clone(manifest.Assets)

	return manifest
}
