// Package resource loads declarative Coding Agent resources from user and
// trusted project scopes.
package resource

import (
	"errors"
	"maps"
	"slices"

	"github.com/rsbin/pips/agent/bundle"
	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
)

var (
	// ErrInvalid reports invalid loader options or resource metadata.
	ErrInvalid = errors.New("coding resource: invalid")
	// ErrLimitExceeded reports bounded discovery or content limits.
	ErrLimitExceeded = errors.New("coding resource: limit exceeded")
	// ErrDuplicate reports an identity collision without defined precedence.
	ErrDuplicate = errors.New("coding resource: duplicate")
	// ErrInsecure reports a writable or otherwise unsafe user resource root.
	ErrInsecure = errors.New("coding resource: insecure root")
)

// Limits bound discovery and reads across direct Skills and Bundle manifests.
type Limits struct {
	MaxEntries            int
	MaxSkillManifests     int
	MaxBundleManifests    int
	MaxDepth              int
	MaxPathBytes          int
	MaxSkillBytes         int64
	MaxTotalSkillBytes    int64
	MaxResourcesPerSkill  int
	MaxResourceBytes      int64
	MaxSkillResourceBytes int64
	MaxTotalResourceBytes int64
	Bundle                bundle.Limits
}

// DefaultLimits returns conservative local resource bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxEntries:            4_096,
		MaxSkillManifests:     256,
		MaxBundleManifests:    64,
		MaxDepth:              12,
		MaxPathBytes:          4 << 10,
		MaxSkillBytes:         1 << 20,
		MaxTotalSkillBytes:    8 << 20,
		MaxResourcesPerSkill:  128,
		MaxResourceBytes:      1 << 20,
		MaxSkillResourceBytes: 4 << 20,
		MaxTotalResourceBytes: 16 << 20,
		Bundle:                bundle.DefaultLimits(),
	}
}

// Options select user and optional trusted-project resource boundaries.
type Options struct {
	Paths          paths.Layout
	Tree           *workspace.Tree
	ProjectTrusted bool
	Limits         Limits
}

// Diagnostic describes a non-fatal resource decision using safe provenance.
type Diagnostic struct {
	Code       string
	Resource   string
	Winner     string
	Suppressed string
	Message    string
}

// Result owns fully decoded resources and no open filesystem handles.
type Result struct {
	direct      []skillEntry
	bundles     []*bundle.Bundle
	diagnostics []Diagnostic
}

type skillEntry struct {
	skill      harness.Skill
	provenance string
	priority   int
	direct     bool
}

// Bundles returns loaded Bundles in deterministic discovery order.
func (r Result) Bundles() []*bundle.Bundle {
	return slices.Clone(r.bundles)
}

// Diagnostics returns load-time diagnostics.
func (r Result) Diagnostics() []Diagnostic {
	return slices.Clone(r.diagnostics)
}

// ResolveSkills combines direct Skills with an activated Extension snapshot.
// Project direct wins over user direct, which wins over Bundle/Extension.
func (r Result) ResolveSkills(entries ...extension.SkillEntry) ([]harness.Skill, []Diagnostic, error) {
	candidates := slices.Clone(r.direct)

	for _, entry := range entries {
		provenance := "extension:" + entry.Origin.ExtensionID
		skill := cloneSkill(entry.Skill)
		skill.Source = provenance
		skill.AllowedTools = nil

		candidates = append(candidates, skillEntry{
			skill:      skill,
			provenance: provenance,
		})
	}

	return resolveSkills(candidates, r.diagnostics)
}

func cloneSkill(skill harness.Skill) harness.Skill {
	skill.Metadata = maps.Clone(skill.Metadata)
	skill.AllowedTools = slices.Clone(skill.AllowedTools)
	skill.Resources = slices.Clone(skill.Resources)

	return skill
}
