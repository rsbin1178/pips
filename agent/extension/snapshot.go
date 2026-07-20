package extension

import (
	"context"
	"slices"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
)

// Snapshot is an immutable view of one successfully activated generation.
// Keep the Activation that produced it leased until every user of the
// Snapshot has finished.
type Snapshot struct {
	generation  uint64
	descriptors []Descriptor
	diagnostics []Diagnostic
	catalog     *catalog.Catalog
	skills      []SkillEntry
	prompts     []PromptEntry
	assets      []AssetEntry
	middleware  []ai.Middleware
	hooks       Hooks
}

// Generation returns the monotonically increasing Runtime generation.
func (s Snapshot) Generation() uint64 { return s.generation }

// Descriptors returns the activated Extension descriptors in registration
// order.
func (s Snapshot) Descriptors() []Descriptor {
	values := make([]Descriptor, len(s.descriptors))
	for i, descriptor := range s.descriptors {
		values[i] = cloneDescriptor(descriptor)
	}

	return values
}

// Diagnostics returns non-fatal activation diagnostics.
func (s Snapshot) Diagnostics() []Diagnostic { return slices.Clone(s.diagnostics) }

// Catalog returns the immutable catalog of Extension tools.
func (s Snapshot) Catalog() *catalog.Catalog { return s.catalog }

// Skills returns the activated Harness skills without provenance wrappers.
func (s Snapshot) Skills() []harness.Skill {
	values := make([]harness.Skill, len(s.skills))
	for i, entry := range s.skills {
		values[i] = cloneSkill(entry.Skill)
	}

	return values
}

// SkillEntries returns activated Skills with Extension provenance.
func (s Snapshot) SkillEntries() []SkillEntry {
	values := make([]SkillEntry, len(s.skills))
	for i, entry := range s.skills {
		values[i] = SkillEntry{Origin: entry.Origin, Skill: cloneSkill(entry.Skill)}
	}

	return values
}

// Prompts returns the activated prompt templates without provenance wrappers.
func (s Snapshot) Prompts() []harness.PromptTemplate {
	values := make([]harness.PromptTemplate, len(s.prompts))
	for i, entry := range s.prompts {
		values[i] = entry.Template
	}

	return values
}

// PromptEntries returns prompt templates with Extension provenance.
func (s Snapshot) PromptEntries() []PromptEntry { return slices.Clone(s.prompts) }

// Assets returns opaque typed assets with Extension provenance and defensive
// copies of their data.
func (s Snapshot) Assets() []AssetEntry {
	values := make([]AssetEntry, len(s.assets))
	for i, entry := range s.assets {
		values[i] = AssetEntry{Origin: entry.Origin, Asset: cloneAsset(entry.Asset)}
	}

	return values
}

// Hooks returns the composed Agent hooks for this generation.
func (s Snapshot) Hooks() Hooks { return s.hooks }

// Model wraps model with activated AI middleware in registration order.
func (s Snapshot) Model(model ai.LanguageModel) ai.LanguageModel {
	return ai.Chain(model, s.middleware...)
}

// AgentOptions authorizes Extension tools and returns the complete Agent
// option set for this Snapshot.
func (s Snapshot) AgentOptions(
	ctx context.Context,
	policy catalog.Policy,
) ([]agent.Option, error) {
	if s.catalog == nil {
		return nil, ErrNotActive
	}

	tools, err := s.catalog.Snapshot(ctx, policy)
	if err != nil {
		return nil, err
	}

	opts := []agent.Option{agent.WithTools(tools...)}
	opts = append(opts, s.hooks.AgentOptions()...)

	return opts, nil
}

// HarnessOptions authorizes Extension tools and returns Harness resources and
// Agent hooks. The Harness owns its event recorder, so observers are installed
// through harness.WithOnEvent rather than agent.WithOnEvent.
func (s Snapshot) HarnessOptions(
	ctx context.Context,
	policy catalog.Policy,
) ([]harness.Option, error) {
	if s.catalog == nil {
		return nil, ErrNotActive
	}

	tools, err := s.catalog.Snapshot(ctx, policy)
	if err != nil {
		return nil, err
	}

	opts := []harness.Option{
		harness.WithTools(tools...),
		harness.WithSkills(s.Skills()...),
		harness.WithTemplates(s.Prompts()...),
		harness.WithAgentOptions(s.hooks.agentOptions(false)...),
	}
	if s.hooks.Observe != nil {
		opts = append(opts, harness.WithOnEvent(s.hooks.Observe))
	}

	return opts, nil
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.descriptors = snapshot.Descriptors()
	snapshot.diagnostics = snapshot.Diagnostics()
	snapshot.skills = snapshot.SkillEntries()
	snapshot.prompts = snapshot.PromptEntries()
	snapshot.assets = snapshot.Assets()
	snapshot.middleware = slices.Clone(snapshot.middleware)

	return snapshot
}
