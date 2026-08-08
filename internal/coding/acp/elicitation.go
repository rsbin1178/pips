package acp

import (
	"encoding/json"
	"fmt"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin/pips/internal/coding/question"
)

const (
	jsonTypeString          = "string"
	elicitationActionAccept = "accept"
)

// These types isolate the stable ACP v1 elicitation wire shape from the SDK's
// legacy Unstable-prefixed representation, which omits session scope.
type createElicitationRequest struct {
	Mode            string             `json:"mode"`
	SessionID       acpsdk.SessionId   `json:"sessionId"`
	ToolCallID      *acpsdk.ToolCallId `json:"toolCallId,omitempty"`
	Message         string             `json:"message"`
	RequestedSchema elicitationSchema  `json:"requestedSchema"`
}

type elicitationSchema struct {
	Type       string                         `json:"type"`
	Title      string                         `json:"title,omitempty"`
	Properties map[string]elicitationProperty `json:"properties"`
	Required   []string                       `json:"required,omitempty"`
}

type elicitationProperty struct {
	Type        string               `json:"type"`
	Title       string               `json:"title,omitempty"`
	Description string               `json:"description,omitempty"`
	Enum        []string             `json:"enum,omitempty"`
	Items       *elicitationProperty `json:"items,omitempty"`
}

type createElicitationResponse struct {
	Action  string                     `json:"action"`
	Content map[string]json.RawMessage `json:"content,omitempty"`
}

func elicitationForQuestion(
	sessionID string,
	request question.Request,
) (createElicitationRequest, error) {
	if err := question.ValidateRequest(request); err != nil {
		return createElicitationRequest{}, fmt.Errorf("%w: invalid question request", ErrInvalid)
	}

	toolCallID := acpsdk.ToolCallId(request.ToolCallID)

	result := createElicitationRequest{
		Mode: "form", SessionID: acpsdk.SessionId(sessionID), ToolCallID: &toolCallID,
		RequestedSchema: elicitationSchema{
			Type: "object", Title: "Additional information",
			Properties: make(map[string]elicitationProperty),
		},
	}
	if request.Kind == question.RequestFreeform {
		result.Message = request.Prompt
		result.RequestedSchema.Properties["response"] = elicitationProperty{
			Type: jsonTypeString, Title: "Response",
		}
		result.RequestedSchema.Required = []string{"response"}

		return result, nil
	}

	result.Message = "Please answer the following questions."

	for index, item := range request.Questions {
		key := questionKey(index)
		labels := make([]string, len(item.Options))

		descriptions := make([]string, len(item.Options))
		for optionIndex, option := range item.Options {
			labels[optionIndex] = option.Label
			descriptions[optionIndex] = option.Label + ": " + option.Description
		}

		labels = append(labels, customChoiceLabel(item))

		property := elicitationProperty{
			Type: jsonTypeString, Title: item.Header,
			Description: item.Question + "\n\n" + strings.Join(descriptions, "\n"),
			Enum:        labels,
		}
		if item.Multiple {
			property.Type = "array"
			property.Enum = nil
			property.Items = &elicitationProperty{Type: jsonTypeString, Enum: labels}
		}

		result.RequestedSchema.Properties[key] = property
		result.RequestedSchema.Properties[key+"_custom"] = elicitationProperty{
			Type: jsonTypeString, Title: item.Header + " — custom answer",
		}
		result.RequestedSchema.Required = append(result.RequestedSchema.Required, key)
	}

	return result, nil
}

func resolutionFromElicitation(
	request question.Request,
	response createElicitationResponse,
) (question.Resolution, bool, error) {
	switch response.Action {
	case "decline", "cancel":
		return question.Resolution{}, false, nil
	case elicitationActionAccept:
	default:
		return question.Resolution{}, false, fmt.Errorf("%w: unsupported elicitation action", ErrInvalid)
	}

	resolution := question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
	}
	if request.Kind == question.RequestFreeform {
		if err := populateFreeformResolution(response.Content, &resolution); err != nil {
			return question.Resolution{}, false, err
		}
	} else {
		if err := populateStructuredResolution(request, response.Content, &resolution); err != nil {
			return question.Resolution{}, false, err
		}
	}

	if err := question.ValidateResolution(request, resolution); err != nil {
		return question.Resolution{}, false, fmt.Errorf("%w: invalid elicitation response", ErrInvalid)
	}

	return resolution, true, nil
}

func populateFreeformResolution(
	content map[string]json.RawMessage,
	resolution *question.Resolution,
) error {
	if err := json.Unmarshal(content["response"], &resolution.Chat); err != nil {
		return fmt.Errorf("%w: malformed free-form response", ErrInvalid)
	}

	return nil
}

func populateStructuredResolution(
	request question.Request,
	content map[string]json.RawMessage,
	resolution *question.Resolution,
) error {
	resolution.Answers = make([]question.Answer, len(request.Questions))
	for index, item := range request.Questions {
		key := questionKey(index)

		custom, customSet, err := optionalString(content, key+"_custom")
		if err != nil {
			return err
		}

		if customSet && custom != "" {
			resolution.Answers[index].Custom = custom

			continue
		}

		if err := populateSelections(content[key], item.Multiple, &resolution.Answers[index]); err != nil {
			return err
		}
	}

	return nil
}

func populateSelections(raw json.RawMessage, multiple bool, answer *question.Answer) error {
	if multiple {
		if err := json.Unmarshal(raw, &answer.Selections); err != nil {
			return fmt.Errorf("%w: malformed multiple selection", ErrInvalid)
		}

		return nil
	}

	var selected string
	if err := json.Unmarshal(raw, &selected); err != nil {
		return fmt.Errorf("%w: malformed selection", ErrInvalid)
	}

	answer.Selections = []string{selected}

	return nil
}

func optionalString(
	content map[string]json.RawMessage,
	key string,
) (string, bool, error) {
	raw, exists := content[key]
	if !exists || len(raw) == 0 || string(raw) == "null" {
		return "", false, nil
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, fmt.Errorf("%w: malformed custom response", ErrInvalid)
	}

	return value, true, nil
}

func questionKey(index int) string {
	return fmt.Sprintf("question_%d", index+1)
}

func customChoiceLabel(item question.Question) string {
	label := "Other (write in)"

	for suffix := 2; ; suffix++ {
		collision := false

		for _, option := range item.Options {
			if option.Label == label {
				collision = true
				break
			}
		}

		if !collision {
			return label
		}

		label = fmt.Sprintf("Other (write in %d)", suffix)
	}
}
