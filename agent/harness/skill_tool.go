//nolint:wsl_v5 // Dynamic Tool schema and strict execution keep validation steps adjacent.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

// SkillToolName is the reserved Tool name for explicit Skill activation.
const SkillToolName = "skill"

type skillToolArguments struct {
	Name     string `json:"name"`
	Resource string `json:"resource,omitempty"`
}

type skillToolResult struct {
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	Instructions  string                 `json:"instructions"`
	License       string                 `json:"license,omitempty"`
	Compatibility string                 `json:"compatibility,omitempty"`
	Metadata      map[string]string      `json:"metadata,omitempty"`
	Resources     []skillResourceSummary `json:"resources,omitempty"`
}

type skillResourceSummary struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Text bool   `json:"text"`
}

type skillResourceResult struct {
	Name     string `json:"name"`
	Resource string `json:"resource"`
	Content  string `json:"content"`
}

type skillTool struct {
	catalog *SkillCatalog
	decl    ai.Tool
}

// NewSkillTool returns the reserved Tool that activates exact names from
// catalog. The dynamic name enum prevents the model from guessing unavailable
// skills. An optional resource path reads one bounded text resource after the
// skill has been activated.
//
// The Tool never exposes Source, executes scripts, interprets allowed-tools,
// reads binary resources, or changes policy.
func NewSkillTool(catalog *SkillCatalog) (agent.Tool, error) {
	if catalog == nil {
		return nil, errors.New("harness: nil skill catalog")
	}

	names := make([]any, 0, len(catalog.list))
	for _, skill := range catalog.list {
		names = append(names, skill.Name)
	}

	return &skillTool{
		catalog: catalog,
		decl: ai.Tool{
			Name:        SkillToolName,
			Description: "Load one exact skill's instructions or one of its text resources.",
			InputSchema: &ai.Schema{
				Type: "object",
				Properties: map[string]*ai.Schema{
					string(KindName): {
						Type:        "string",
						Description: "Exact skill name from the available-skills list.",
						Enum:        names,
					},
					"resource": {
						Type:        "string",
						Description: "Optional exact text resource path returned by a prior activation.",
					},
				},
				Required:             []string{string(KindName)},
				AdditionalProperties: false,
			},
		},
	}, nil
}

func (t *skillTool) Decl() ai.Tool {
	return t.decl
}

func (t *skillTool) Exec(_ context.Context, call agent.ToolCall) ([]ai.Part, error) {
	arguments, err := decodeSkillToolArguments(call.Args)
	if err != nil {
		return nil, err
	}

	activation, err := t.catalog.Activate(arguments.Name)
	if err != nil {
		return nil, err
	}

	if arguments.Resource != "" {
		return encodeSkillResource(activation.Skill, arguments.Resource)
	}

	resources := make([]skillResourceSummary, 0, len(activation.Skill.Resources))
	for _, resource := range activation.Skill.Resources {
		resources = append(resources, skillResourceSummary{
			Path: resource.Path,
			Size: resource.Size,
			Text: resource.Text,
		})
	}

	return encodeSkillToolResult(skillToolResult{
		Name:          activation.Skill.Name,
		Description:   activation.Skill.Description,
		Instructions:  activation.Skill.Content,
		License:       activation.Skill.License,
		Compatibility: activation.Skill.Compatibility,
		Metadata:      maps.Clone(activation.Skill.Metadata),
		Resources:     resources,
	})
}

func decodeSkillToolArguments(raw ai.JSON) (skillToolArguments, error) {
	var arguments skillToolArguments

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&arguments); err != nil {
		return skillToolArguments{}, fmt.Errorf("harness: invalid skill arguments: %w", err)
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}

		return skillToolArguments{}, fmt.Errorf("harness: invalid skill arguments: %w", err)
	}

	return arguments, nil
}

func encodeSkillResource(skill Skill, resourcePath string) ([]ai.Part, error) {
	for _, resource := range skill.Resources {
		if resource.Path != resourcePath {
			continue
		}

		if !resource.Text {
			return nil, fmt.Errorf(
				"harness: skill %q resource %q is not a text resource",
				skill.Name,
				resourcePath,
			)
		}

		return encodeSkillToolResult(skillResourceResult{
			Name:     skill.Name,
			Resource: resource.Path,
			Content:  resource.Content,
		})
	}

	return nil, fmt.Errorf(
		"harness: skill %q has unknown resource %q",
		skill.Name,
		resourcePath,
	)
}

func encodeSkillToolResult(value any) ([]ai.Part, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("harness: encode skill activation: %w", err)
	}

	return agent.TextResult(string(encoded)), nil
}
