package harness

import (
	"strconv"
	"strings"

	"github.com/rsbin/pips/ai"
)

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
	// Source is the path the skill was loaded from (see [LoadSkills]); when
	// set it is advertised to the model so file-capable agents can read the
	// full instructions on demand.
	Source string
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

	b.WriteString("The following skills provide specialized instructions:\n\n<available-skills>\n")

	for _, s := range skills {
		b.WriteString("<skill>\n<name>" + s.Name + "</name>\n")
		b.WriteString("<description>" + s.Description + "</description>\n")

		if s.Source != "" {
			b.WriteString("<location>" + s.Source + "</location>\n")
		}

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
