package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/rsbin/pips/agent"
)

// SkillToolName is the reserved Tool name for explicit Skill activation.
const SkillToolName = "skill"

type skillToolArguments struct {
	Name string `json:"name" description:"Exact skill name from the available-skills list."`
}

type skillToolResult struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Instructions  string            `json:"instructions"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// NewSkillTool returns the reserved Tool that activates exact names from
// catalog. The Tool exposes full instructions only after activation; it never
// exposes Source, executes scripts, interprets allowed-tools, or changes policy.
func NewSkillTool(catalog *SkillCatalog) (agent.Tool, error) {
	if catalog == nil {
		return nil, errors.New("harness: nil skill catalog")
	}

	return agent.NewTool(
		SkillToolName,
		"Load the full instructions for one exact skill name from the available-skills list.",
		func(_ context.Context, arguments skillToolArguments) (string, error) {
			activation, err := catalog.Activate(arguments.Name)
			if err != nil {
				return "", err
			}

			encoded, err := json.Marshal(skillToolResult{
				Name:          activation.Skill.Name,
				Description:   activation.Skill.Description,
				Instructions:  activation.Skill.Content,
				License:       activation.Skill.License,
				Compatibility: activation.Skill.Compatibility,
				Metadata:      maps.Clone(activation.Skill.Metadata),
			})
			if err != nil {
				return "", fmt.Errorf("harness: encode skill activation: %w", err)
			}

			return string(encoded), nil
		},
	), nil
}
