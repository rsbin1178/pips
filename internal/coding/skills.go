//nolint:wsl_v5 // Snapshot projection and exact-token parsing keep validation adjacent.
package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
)

// SkillSource is a safe, stable source label for a discovered Skill.
type SkillSource string

// Skill source labels exposed to product frontends.
const (
	SkillSourceProjectPips   SkillSource = "project_pips"
	SkillSourceProjectAgents SkillSource = "project_agents"
	SkillSourceUserPips      SkillSource = "user_pips"
	SkillSourceUserAgents    SkillSource = "user_agents"
	SkillSourceExtension     SkillSource = "extension"
	SkillSourceUnknown       SkillSource = "unknown"
)

// SkillSummary is the content-free product projection of one Skill.
type SkillSummary struct {
	Name           string      `json:"name"`
	Description    string      `json:"description"`
	Source         SkillSource `json:"source"`
	UserInvocable  bool        `json:"user_invocable"`
	ModelInvocable bool        `json:"model_invocable"`
	ResourceCount  int         `json:"resource_count"`
}

// SkillDiagnostic is a safe, non-fatal discovery or compatibility decision.
type SkillDiagnostic struct {
	Code     string `json:"code"`
	Resource string `json:"resource,omitempty"`
	Message  string `json:"message,omitempty"`
}

// SkillSnapshot is an immutable point-in-time projection for frontends.
type SkillSnapshot struct {
	Skills      []SkillSummary    `json:"skills"`
	Diagnostics []SkillDiagnostic `json:"diagnostics,omitempty"`
}

// Clone returns a defensive copy.
func (s SkillSnapshot) Clone() SkillSnapshot {
	s.Skills = slices.Clone(s.Skills)
	s.Diagnostics = slices.Clone(s.Diagnostics)

	return s
}

// Skills returns the current resolved Skill generation without exposing full
// instructions or filesystem paths. A concurrent reload wins the Runtime
// operation lease, so callers never observe mixed resource generations.
func (r *Runtime) Skills(ctx context.Context) (_ SkillSnapshot, returnErr error) {
	if r == nil {
		return SkillSnapshot{}, ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return SkillSnapshot{}, err
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return SkillSnapshot{}, stateError("skills", phase, ErrRuntimeClosed)
	}
	if r.active != nil {
		phase := r.state.Phase
		r.mu.Unlock()

		return SkillSnapshot{}, stateError("skills", phase, ErrRuntimeBusy)
	}

	activation, err := r.extensions.Acquire()
	resources := r.resources
	r.mu.Unlock()
	if err != nil {
		return SkillSnapshot{}, err
	}
	defer func() {
		returnErr = errors.Join(
			returnErr,
			activation.Release(context.WithoutCancel(ctx)),
		)
	}()

	skills, diagnostics, err := resources.ResolveSkills(activation.Snapshot().SkillEntries()...)
	if err != nil {
		return SkillSnapshot{}, err
	}

	result := SkillSnapshot{
		Skills:      make([]SkillSummary, 0, len(skills)),
		Diagnostics: make([]SkillDiagnostic, 0, len(diagnostics)),
	}
	for _, skill := range skills {
		result.Skills = append(result.Skills, SkillSummary{
			Name:           skill.Name,
			Description:    skill.Description,
			Source:         skillSource(skill.Source),
			UserInvocable:  skill.UserInvocable(),
			ModelInvocable: skill.ModelInvocable(),
			ResourceCount:  len(skill.Resources),
		})
	}
	for _, diagnostic := range diagnostics {
		result.Diagnostics = append(result.Diagnostics, SkillDiagnostic{
			Code: diagnostic.Code, Resource: diagnostic.Resource, Message: diagnostic.Message,
		})
	}

	return result, nil
}

func skillSource(source string) SkillSource {
	switch {
	case strings.HasPrefix(source, "project:pips/"):
		return SkillSourceProjectPips
	case strings.HasPrefix(source, "project:agents/"):
		return SkillSourceProjectAgents
	case strings.HasPrefix(source, "user:pips/"):
		return SkillSourceUserPips
	case strings.HasPrefix(source, "user:agents/"):
		return SkillSourceUserAgents
	case strings.HasPrefix(source, "extension:"):
		return SkillSourceExtension
	default:
		return SkillSourceUnknown
	}
}

func explicitSkillNames(messages []ai.Message, catalog *harness.SkillCatalog) []string {
	if catalog == nil {
		return nil
	}

	available := make(map[string]struct{})
	for _, skill := range catalog.List() {
		available[skill.Name] = struct{}{}
	}

	seen := make(map[string]struct{})
	var names []string
	for _, message := range messages {
		if message.Role != ai.RoleUser {
			continue
		}

		for _, part := range message.Parts {
			text, ok := part.(ai.TextPart)
			if !ok {
				continue
			}

			for _, candidate := range dollarTokens(text.Text) {
				if _, ok := available[candidate]; !ok {
					continue
				}
				if _, duplicate := seen[candidate]; duplicate {
					continue
				}

				seen[candidate] = struct{}{}
				names = append(names, candidate)
			}
		}
	}

	return names
}

func dollarTokens(text string) []string {
	var tokens []string
	for index := 0; index < len(text); index++ {
		if text[index] != '$' {
			continue
		}
		if index > 0 && isDollarTokenByte(text[index-1]) {
			continue
		}

		end := index + 1
		for end < len(text) && isDollarTokenByte(text[end]) {
			end++
		}
		if end == index+1 {
			continue
		}

		candidate := text[index+1 : end]
		if validSkillReference(candidate) {
			tokens = append(tokens, candidate)
		}
		index = end - 1
	}

	return tokens
}

func isDollarTokenByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-' || value == '_'
}

func validSkillReference(value string) bool {
	if value == "" || len(value) > 64 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}

	previousHyphen := false
	for _, char := range []byte(value) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			previousHyphen = false
		case char == '-' && !previousHyphen:
			previousHyphen = true
		default:
			return false
		}
	}

	return true
}

type explicitSkillPrompt struct {
	Name         string                  `json:"name"`
	Instructions string                  `json:"instructions"`
	Resources    []explicitSkillResource `json:"resources,omitempty"`
}

type explicitSkillResource struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Text bool   `json:"text"`
}

func activateExplicitSkills(
	catalog *harness.SkillCatalog,
	names []string,
) (string, error) {
	if len(names) == 0 {
		return "", nil
	}

	selected := make([]explicitSkillPrompt, 0, len(names))
	for _, name := range names {
		activation, err := catalog.Activate(name)
		if err != nil {
			return "", err
		}

		resources := make([]explicitSkillResource, 0, len(activation.Skill.Resources))
		for _, resource := range activation.Skill.Resources {
			resources = append(resources, explicitSkillResource{
				Path: resource.Path, Size: resource.Size, Text: resource.Text,
			})
		}
		selected = append(selected, explicitSkillPrompt{
			Name: activation.Skill.Name, Instructions: activation.Skill.Content, Resources: resources,
		})
	}

	encoded, err := json.Marshal(selected)
	if err != nil {
		return "", fmt.Errorf("coding skills: encode explicit selection: %w", err)
	}

	return "The user explicitly selected the following Skills for this request. " +
		"Treat their instructions as applicable system context. Resource entries are metadata; " +
		"read text content through the skill tool only when needed.\n\n<explicit_skills_json>\n" +
		string(encoded) + "\n</explicit_skills_json>", nil
}
