package harness

import (
	"fmt"
	"html"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/rsbin/pips/ai"
)

// SkillInvocation controls whether a Skill can be selected by a user, a
// model, both, or neither. The zero value preserves the historical behavior
// and allows both invocation paths.
type SkillInvocation uint8

const (
	// SkillInvocationDefault allows both user and model invocation.
	SkillInvocationDefault SkillInvocation = iota
	// SkillInvocationUserOnly allows explicit user invocation only.
	SkillInvocationUserOnly
	// SkillInvocationModelOnly allows autonomous model invocation only.
	SkillInvocationModelOnly
	// SkillInvocationDisabled keeps the Skill unavailable to both paths.
	SkillInvocationDisabled
)

// SkillResource is one immutable, bounded resource bundled with a Skill.
// Path is slash-relative to the Skill directory. Text content is available
// only when Text is true; binary resources expose metadata but not content.
type SkillResource struct {
	Path    string
	Size    int64
	Text    bool
	Content string
}

// Skill is an instruction resource surfaced to the model through the system
// prompt. Applications own loading skills (from SKILL.md files or anywhere
// else) and pass the parsed values in.
type Skill struct {
	// Name is the stable identifier listed to the model.
	Name string
	// Description tells the model when the skill applies.
	Description string
	// Content is the full instruction text.
	Content string
	// Source records application-owned provenance. Skill discovery and the
	// activation Tool never expose it to the model.
	Source string
	// License is the optional skill license declaration.
	License string
	// Compatibility records optional host or environment requirements.
	Compatibility string
	// Metadata is the optional scalar metadata mapping from the manifest.
	Metadata map[string]string
	// AllowedTools is declarative metadata only. A host must still enforce its
	// own Tool policy; this package never grants execution from a Skill.
	AllowedTools []string
	// Invocation controls user and model discovery. Its zero value allows both.
	Invocation SkillInvocation
	// Resources contains immutable application-validated sibling resources.
	Resources []SkillResource
}

// UserInvocable reports whether a user may explicitly select the Skill.
func (s Skill) UserInvocable() bool {
	return s.Invocation == SkillInvocationDefault || s.Invocation == SkillInvocationUserOnly
}

// ModelInvocable reports whether the model may discover and activate the Skill.
func (s Skill) ModelInvocable() bool {
	return s.Invocation == SkillInvocationDefault || s.Invocation == SkillInvocationModelOnly
}

func cloneSkill(in Skill) Skill {
	out := in
	out.Metadata = maps.Clone(in.Metadata)
	out.AllowedTools = slices.Clone(in.AllowedTools)
	out.Resources = slices.Clone(in.Resources)

	return out
}

func validateSkillResources(skill Skill) error {
	seen := make(map[string]struct{}, len(skill.Resources))

	for _, resource := range skill.Resources {
		if !validSkillResourcePath(resource.Path) {
			return fmt.Errorf("harness: skill %q has invalid resource path %q", skill.Name, resource.Path)
		}

		if resource.Size < 0 {
			return fmt.Errorf("harness: skill %q resource %q has negative size", skill.Name, resource.Path)
		}

		if !resource.Text && resource.Content != "" {
			return fmt.Errorf("harness: skill %q binary resource %q contains text", skill.Name, resource.Path)
		}

		if _, ok := seen[resource.Path]; ok {
			return fmt.Errorf("harness: skill %q has duplicate resource %q", skill.Name, resource.Path)
		}

		seen[resource.Path] = struct{}{}
	}

	return nil
}

func validSkillResourcePath(value string) bool {
	if value == "" || path.IsAbs(value) || path.Clean(value) != value ||
		value == "." || strings.HasPrefix(value, "../") || strings.ContainsRune(value, '\\') {
		return false
	}

	for _, char := range value {
		if char < ' ' || char == '\x7f' {
			return false
		}
	}

	return true
}

// PromptTemplate is a reusable prompt with positional argument placeholders.
type PromptTemplate struct {
	// Name is the stable identifier for lookup.
	Name string
	// Description is optional documentation.
	Description string
	// Content is the template text; see [PromptTemplate.Format].
	Content string
}

// ValidateTemplates checks names, content, and duplicate identities for a
// complete prompt-template set.
func ValidateTemplates(templates ...PromptTemplate) error {
	seen := make(map[string]struct{}, len(templates))
	for _, template := range templates {
		name := strings.TrimSpace(template.Name)
		if name == "" || name != template.Name || len(name) > 128 {
			return fmt.Errorf("harness: invalid prompt template name %q", template.Name)
		}

		if strings.TrimSpace(template.Content) == "" {
			return fmt.Errorf("harness: prompt template %q has empty content", name)
		}

		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("harness: duplicate prompt template %q", name)
		}

		seen[name] = struct{}{}
	}

	return nil
}

// Format substitutes arguments into the template: $1..$9 reference
// positional arguments, $ARGUMENTS expands to all of them space-joined.
// Templates without placeholders get the arguments appended.
func (t PromptTemplate) Format(args ...string) string {
	out := t.Content
	used := false

	if strings.Contains(out, "$ARGUMENTS") {
		out = strings.ReplaceAll(out, "$ARGUMENTS", strings.Join(args, " "))
		used = true
	}

	for i, arg := range args {
		if i >= 9 {
			break
		}

		placeholder := "$" + strconv.Itoa(i+1)
		if strings.Contains(out, placeholder) {
			out = strings.ReplaceAll(out, placeholder, arg)
			used = true
		}
	}

	if !used && len(args) > 0 {
		out += "\n\n" + strings.Join(args, " ")
	}

	return out
}

// FormatSkillsPrompt renders skills as the system-prompt block the model
// reads to know which skills exist (agentskills.io style). It returns ""
// when there are no skills.
func FormatSkillsPrompt(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("The following skills provide specialized instructions. " +
		"Use the \"" + SkillToolName + "\" tool with an exact skill name to load its full instructions.\n\n" +
		"<available-skills>\n")

	for _, s := range skills {
		b.WriteString("<skill>\n<name>")
		b.WriteString(s.Name)
		b.WriteString("</name>\n<description>")
		b.WriteString(html.EscapeString(s.Description))
		b.WriteString("</description>\n")

		b.WriteString("</skill>\n")
	}

	b.WriteString("</available-skills>")

	return b.String()
}

// SystemContext is what a [WithSystemFunc] callback sees when assembling the
// system prompt for a prompt run.
type SystemContext struct {
	// Model serves the upcoming run.
	Model ai.LanguageModel
	// Skills and Templates are the harness's configured resources.
	Skills    []Skill
	Templates []PromptTemplate
	// Session is the conversation being continued.
	Session *Session
}
