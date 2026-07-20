package extension

import (
	"context"
	"maps"
	"slices"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
)

// Capability is one stable feature name understood by a Runtime or Bundle.
type Capability string

// Capabilities implemented directly by this package.
const (
	CapabilityTools       Capability = "agent.tools"
	CapabilityHooks       Capability = "agent.hooks"
	CapabilitySkills      Capability = "harness.skills"
	CapabilityPrompts     Capability = "harness.prompts"
	CapabilityMiddleware  Capability = "ai.middleware"
	CapabilityLifecycle   Capability = "extension.lifecycle"
	CapabilityTypedAssets Capability = "extension.assets"
)

// Capabilities is an immutable set of feature names. Its zero value is an
// empty set.
type Capabilities struct {
	values map[Capability]struct{}
}

// NewCapabilities creates a set, ignoring duplicate values.
func NewCapabilities(values ...Capability) Capabilities {
	set := Capabilities{values: make(map[Capability]struct{}, len(values))}
	for _, value := range values {
		if value != "" {
			set.values[value] = struct{}{}
		}
	}

	return set
}

// Has reports whether capability is present.
func (c Capabilities) Has(capability Capability) bool {
	_, ok := c.values[capability]

	return ok
}

// List returns sorted capability names.
func (c Capabilities) List() []Capability {
	values := slices.Collect(maps.Keys(c.values))
	slices.Sort(values)

	return values
}

func (c Capabilities) clone() Capabilities {
	return Capabilities{values: maps.Clone(c.values)}
}

func builtInCapabilities() Capabilities {
	return NewCapabilities(
		CapabilityTools,
		CapabilityHooks,
		CapabilitySkills,
		CapabilityPrompts,
		CapabilityMiddleware,
		CapabilityLifecycle,
		CapabilityTypedAssets,
	)
}

// Descriptor identifies one compiled Extension and its capability contract.
type Descriptor struct {
	ID       string
	Version  string
	Requires []Capability
	Optional []Capability
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.Requires = slices.Clone(descriptor.Requires)
	descriptor.Optional = slices.Clone(descriptor.Optional)

	return descriptor
}

// Origin records which Extension contributed a runtime value.
type Origin struct {
	ExtensionID string
	Version     string
}

// Severity classifies a non-fatal activation diagnostic.
type Severity uint8

// Diagnostic severities.
const (
	SeverityUnknown Severity = iota
	SeverityWarning
)

// Diagnostic reports an optional capability or resource condition that did
// not prevent activation.
type Diagnostic struct {
	Severity   Severity
	Origin     Origin
	Capability Capability
	Message    string
}

// Extension prepares one declarative Contribution. Prepare must not start
// long-lived resources; use Contribution.Lifecycle for that work.
type Extension interface {
	Descriptor() Descriptor
	Prepare(context.Context) (Contribution, error)
}

// Lifecycle owns long-lived resources for one Extension generation.
type Lifecycle interface {
	Start(context.Context) error
	Stop(context.Context) error
}

// Tool is one Agent tool plus the policy metadata assigned before it enters a
// Catalog. Runtime supplies Extension provenance from the Descriptor.
type Tool struct {
	Value agent.Tool
	Risk  catalog.Risk
	Tags  []string
}

// Asset is opaque, typed data for an optional application adapter. Core
// extension code never interprets or executes its Data.
type Asset struct {
	Kind      string
	Name      string
	MediaType string
	Source    string
	Data      []byte
}

// AssetEntry attaches immutable Extension provenance to an Asset.
type AssetEntry struct {
	Origin Origin
	Asset  Asset
}

// SkillEntry attaches immutable Extension provenance to a Harness Skill.
type SkillEntry struct {
	Origin Origin
	Skill  harness.Skill
}

// PromptEntry attaches immutable Extension provenance to a prompt template.
type PromptEntry struct {
	Origin   Origin
	Template harness.PromptTemplate
}

// Contribution is the complete declarative output of one Extension.
type Contribution struct {
	Tools      []Tool
	Skills     []harness.Skill
	Prompts    []harness.PromptTemplate
	Assets     []Asset
	Middleware []ai.Middleware
	Hooks      Hooks
	Lifecycle  Lifecycle
}

func cloneSkill(skill harness.Skill) harness.Skill {
	skill.Metadata = maps.Clone(skill.Metadata)
	skill.AllowedTools = slices.Clone(skill.AllowedTools)

	return skill
}

func cloneAsset(asset Asset) Asset {
	asset.Data = slices.Clone(asset.Data)

	return asset
}
