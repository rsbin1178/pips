//nolint:wsl_v5 // Catalog defensive-copy and filtering steps intentionally remain adjacent.
package harness

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// SkillCatalog is an immutable, indexed collection of validated skills. It
// separates low-cost discovery (List and Search) from explicit activation,
// which is the only operation that exposes a skill's full instructions.
//
// A catalog never executes scripts, expands dynamic content, or changes the
// agent's tool permissions. Those are host responsibilities.
type SkillCatalog struct {
	byName map[string]Skill
	list   []Skill

	mu          sync.Mutex
	activations []SkillActivation
}

// SkillActivation records an explicit request for one skill. The record is
// useful for application audit trails and for making full-content injection a
// deliberate operation rather than an incidental side effect of discovery.
type SkillActivation struct {
	Skill       Skill
	ActivatedAt time.Time
}

// NewSkillCatalog validates and indexes skills. Names must be unique and use
// the Agent Skills identifier grammar. Inputs and values returned by the
// catalog are defensively copied.
func NewSkillCatalog(skills ...Skill) (*SkillCatalog, error) {
	catalog := &SkillCatalog{
		byName: make(map[string]Skill, len(skills)),
		list:   make([]Skill, 0, len(skills)),
	}

	for _, skill := range skills {
		if err := validateSkill(skill); err != nil {
			return nil, err
		}

		if _, exists := catalog.byName[skill.Name]; exists {
			return nil, fmt.Errorf("harness: duplicate skill %q", skill.Name)
		}

		cloned := cloneSkill(skill)
		catalog.byName[cloned.Name] = cloned
		catalog.list = append(catalog.list, cloned)
	}

	slices.SortFunc(catalog.list, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })

	return catalog, nil
}

// List returns skill discovery metadata and does not include full content.
func (c *SkillCatalog) List() []Skill {
	if c == nil {
		return nil
	}

	list := make([]Skill, len(c.list))
	for i, skill := range c.list {
		list[i] = discoverySkill(skill)
	}

	return list
}

// Search returns discovery metadata for skills whose name, description, or
// metadata contain every case-insensitive query term. An empty query lists all
// skills. Results are ordered by exact-name match, then prefix match, then
// stable name order.
func (c *SkillCatalog) Search(query string) []Skill {
	if c == nil {
		return nil
	}

	terms := strings.Fields(strings.ToLower(query))

	matches := make([]Skill, 0, len(c.list))
	for _, skill := range c.list {
		if skillMatches(skill, terms) {
			matches = append(matches, discoverySkill(skill))
		}
	}

	slices.SortFunc(matches, func(a, b Skill) int {
		return compareSkillMatch(a, b, strings.ToLower(strings.TrimSpace(query)))
	})

	return matches
}

// ForUser returns a catalog containing only skills that permit explicit user
// invocation. Full instructions remain protected by Activate.
func (c *SkillCatalog) ForUser() (*SkillCatalog, error) {
	return c.filter(func(skill Skill) bool { return skill.UserInvocable() })
}

// ForModel returns a catalog containing only skills that permit autonomous
// model discovery and activation. Full instructions remain protected by
// Activate.
func (c *SkillCatalog) ForModel() (*SkillCatalog, error) {
	return c.filter(func(skill Skill) bool { return skill.ModelInvocable() })
}

// ForModelWith returns the model-invocable catalog plus explicitly named
// skills. It lets an application honor user-only selections for one request
// without making every user-only skill autonomously available to the model.
func (c *SkillCatalog) ForModelWith(names ...string) (*SkillCatalog, error) {
	selected := make(map[string]struct{}, len(names))
	for _, name := range names {
		if c == nil {
			return nil, errors.New("harness: nil skill catalog")
		}
		if _, ok := c.byName[name]; !ok {
			return nil, fmt.Errorf("harness: unknown skill %q", name)
		}
		selected[name] = struct{}{}
	}

	return c.filter(func(skill Skill) bool {
		_, explicitlySelected := selected[skill.Name]

		return skill.ModelInvocable() || explicitlySelected
	})
}

// Resource returns one exact resource from a skill. Callers must explicitly
// request both the skill and slash-relative resource path. Binary resources
// contain metadata only; applications decide whether and how to expose them.
func (c *SkillCatalog) Resource(name, resourcePath string) (SkillResource, error) {
	if c == nil {
		return SkillResource{}, errors.New("harness: nil skill catalog")
	}

	skill, ok := c.byName[name]
	if !ok {
		return SkillResource{}, fmt.Errorf("harness: unknown skill %q", name)
	}

	for _, resource := range skill.Resources {
		if resource.Path == resourcePath {
			return resource, nil
		}
	}

	return SkillResource{}, fmt.Errorf(
		"harness: skill %q has unknown resource %q",
		name,
		resourcePath,
	)
}

// Activate returns a skill's full content and appends an in-memory audit
// record. Hosts that persist audit data should copy Activations after a run.
func (c *SkillCatalog) Activate(name string) (SkillActivation, error) {
	if c == nil {
		return SkillActivation{}, errors.New("harness: nil skill catalog")
	}

	skill, ok := c.byName[name]
	if !ok {
		return SkillActivation{}, fmt.Errorf("harness: unknown skill %q", name)
	}

	record := SkillActivation{Skill: cloneSkill(skill), ActivatedAt: time.Now().UTC()}

	c.mu.Lock()
	c.activations = append(c.activations, record)
	c.mu.Unlock()

	return cloneActivation(record), nil
}

// Activations returns a snapshot of activation records in activation order.
func (c *SkillCatalog) Activations() []SkillActivation {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	records := make([]SkillActivation, len(c.activations))
	for i, record := range c.activations {
		records[i] = cloneActivation(record)
	}

	return records
}

func validateSkill(skill Skill) error {
	if !skillNamePattern.MatchString(skill.Name) || len(skill.Name) > 64 {
		return fmt.Errorf("harness: skill name %q must be 1-64 lowercase letters, numbers, or single hyphens", skill.Name)
	}

	if strings.TrimSpace(skill.Description) == "" || len(skill.Description) > 1024 {
		return fmt.Errorf("harness: skill %q description must be non-empty and at most 1024 bytes", skill.Name)
	}

	if skill.Invocation > SkillInvocationDisabled {
		return fmt.Errorf("harness: skill %q has invalid invocation policy", skill.Name)
	}

	if err := validateSkillResources(skill); err != nil {
		return err
	}

	return nil
}

func discoverySkill(skill Skill) Skill {
	skill = cloneSkill(skill)
	skill.Content = ""
	for i := range skill.Resources {
		skill.Resources[i].Content = ""
	}

	return skill
}

func (c *SkillCatalog) filter(keep func(Skill) bool) (*SkillCatalog, error) {
	if c == nil {
		return nil, errors.New("harness: nil skill catalog")
	}

	skills := make([]Skill, 0, len(c.list))
	for _, skill := range c.list {
		if keep(skill) {
			skills = append(skills, cloneSkill(skill))
		}
	}

	return NewSkillCatalog(skills...)
}

func cloneActivation(record SkillActivation) SkillActivation {
	record.Skill = cloneSkill(record.Skill)

	return record
}

func skillMatches(skill Skill, terms []string) bool {
	if len(terms) == 0 {
		return true
	}

	var fields strings.Builder
	fields.WriteString(skill.Name)
	fields.WriteByte(' ')
	fields.WriteString(skill.Description)

	for key, value := range skill.Metadata {
		fields.WriteByte(' ')
		fields.WriteString(key)
		fields.WriteByte(' ')
		fields.WriteString(value)
	}

	text := strings.ToLower(fields.String())

	for _, term := range terms {
		if !strings.Contains(text, term) {
			return false
		}
	}

	return true
}

func compareSkillMatch(a, b Skill, query string) int {
	aName, bName := strings.ToLower(a.Name), strings.ToLower(b.Name)

	aRank, bRank := skillMatchRank(aName, query), skillMatchRank(bName, query)
	if aRank != bRank {
		return aRank - bRank
	}

	return strings.Compare(a.Name, b.Name)
}

func skillMatchRank(name, query string) int {
	switch {
	case name == query:
		return 0
	case strings.HasPrefix(name, query):
		return 1
	default:
		return 2
	}
}

// CopyMetadata returns a mutable copy of metadata for hosts that need to add
// their own fields without mutating a catalog value.
func (s Skill) CopyMetadata() map[string]string {
	return maps.Clone(s.Metadata)
}
