// Package question defines provider-neutral structured user questions.
package question

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	minQuestionCount    = 1
	maxQuestionCount    = 4
	minOptionCount      = 2
	maxOptionCount      = 4
	maxHeaderBytes      = 64
	maxQuestionBytes    = 4096
	maxLabelBytes       = 128
	maxDescriptionBytes = 1024
	maxPreviewBytes     = 16 << 10
	maxCustomBytes      = 4096
	maxChatBytes        = 16 << 10
	maxFreeformBytes    = 4096
)

// ErrInvalid means a question schema or response violates the protocol.
var ErrInvalid = errors.New("coding question: invalid value")

// Option is one model-provided selectable answer.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

// Question is one single- or multiple-select user question.
type Question struct {
	Header   string   `json:"header"`
	Question string   `json:"question"`
	Options  []Option `json:"options"`
	Multiple bool     `json:"multiple,omitempty"`
}

// Spec is the exact model-supplied ask_user argument shape.
type Spec struct {
	Questions []Question `json:"questions"`
}

// RequestKind distinguishes structured choices from a direct free-form
// response owned by the Runtime.
type RequestKind string

const (
	// RequestStructured is the backward-compatible zero-value request kind.
	RequestStructured RequestKind = ""
	// RequestFreeform asks for one direct text response.
	RequestFreeform RequestKind = "freeform"
)

// Request is one Runtime-owned pending structured question.
type Request struct {
	ID           string      `json:"id"`
	ToolCallID   string      `json:"tool_call_id"`
	SchemaDigest string      `json:"schema_digest"`
	Kind         RequestKind `json:"kind,omitempty"`
	Prompt       string      `json:"prompt,omitempty"`
	Questions    []Question  `json:"questions"`
}

// Answer resolves one question in its original request order.
type Answer struct {
	Selections []string `json:"selections,omitempty"`
	Custom     string   `json:"custom,omitempty"`
}

// Resolution is either complete structured answers or one chat response.
type Resolution struct {
	RequestID    string   `json:"request_id"`
	SchemaDigest string   `json:"schema_digest"`
	Answers      []Answer `json:"answers,omitempty"`
	Chat         string   `json:"chat,omitempty"`
}

// ValidateSpec validates the bounded model-supplied schema.
//
//nolint:gocyclo // Nested schema bounds are intentionally validated in one pass.
func ValidateSpec(spec Spec) error {
	if len(spec.Questions) < minQuestionCount || len(spec.Questions) > maxQuestionCount {
		return fmt.Errorf("%w: expected one to four questions", ErrInvalid)
	}

	headers := make(map[string]struct{}, len(spec.Questions))
	for index, item := range spec.Questions {
		if !validText(item.Header, maxHeaderBytes) {
			return fmt.Errorf("%w: question %d requires a short header", ErrInvalid, index+1)
		}

		if !validText(item.Question, maxQuestionBytes) {
			return fmt.Errorf("%w: question %d requires prompt text", ErrInvalid, index+1)
		}

		if len(item.Options) < minOptionCount || len(item.Options) > maxOptionCount {
			return fmt.Errorf("%w: question %d must have two to four options", ErrInvalid, index+1)
		}

		if _, exists := headers[item.Header]; exists {
			return fmt.Errorf("%w: duplicate question header", ErrInvalid)
		}

		headers[item.Header] = struct{}{}

		labels := make(map[string]struct{}, len(item.Options))
		for optionIndex, option := range item.Options {
			if !validText(option.Label, maxLabelBytes) {
				return fmt.Errorf(
					"%w: question %d option %d requires a short label",
					ErrInvalid,
					index+1,
					optionIndex+1,
				)
			}

			if !validText(option.Description, maxDescriptionBytes) {
				return fmt.Errorf(
					"%w: question %d option %d requires a description",
					ErrInvalid,
					index+1,
					optionIndex+1,
				)
			}

			if !validOptionalText(option.Preview, maxPreviewBytes) {
				return fmt.Errorf(
					"%w: question %d option %d preview is too large or invalid",
					ErrInvalid,
					index+1,
					optionIndex+1,
				)
			}

			if _, exists := labels[option.Label]; exists {
				return fmt.Errorf("%w: question %d has duplicate option labels", ErrInvalid, index+1)
			}

			labels[option.Label] = struct{}{}
		}
	}

	return nil
}

// Digest returns the deterministic digest of one validated schema.
func Digest(spec Spec) (string, error) {
	if err := ValidateSpec(spec); err != nil {
		return "", err
	}

	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("%w: encode schema: %w", ErrInvalid, err)
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

// NewRequest binds a validated schema to Runtime and Tool-call identities.
func NewRequest(id, toolCallID string, spec Spec) (Request, error) {
	if !validIdentity(id) || !validIdentity(toolCallID) {
		return Request{}, fmt.Errorf("%w: invalid request identity", ErrInvalid)
	}

	digest, err := Digest(spec)
	if err != nil {
		return Request{}, err
	}

	return Request{
		ID: id, ToolCallID: toolCallID, SchemaDigest: digest,
		Questions: cloneQuestions(spec.Questions),
	}, nil
}

// NewFreeformRequest binds one bounded direct prompt to Runtime and Tool-call
// identities. The prompt is application-owned when malformed fallback
// arguments cannot be trusted.
func NewFreeformRequest(id, toolCallID, prompt string) (Request, error) {
	if !validIdentity(id) || !validIdentity(toolCallID) || !validText(prompt, maxFreeformBytes) {
		return Request{}, fmt.Errorf("%w: invalid free-form request", ErrInvalid)
	}

	sum := sha256.Sum256([]byte(string(RequestFreeform) + "\x00" + prompt))

	return Request{
		ID: id, ToolCallID: toolCallID, SchemaDigest: hex.EncodeToString(sum[:]),
		Kind: RequestFreeform, Prompt: prompt,
	}, nil
}

// ValidateRequest verifies identity and schema integrity.
func ValidateRequest(request Request) error {
	if !validIdentity(request.ID) || !validIdentity(request.ToolCallID) ||
		len(request.SchemaDigest) != sha256.Size*2 {
		return fmt.Errorf("%w: invalid request identity", ErrInvalid)
	}

	if request.Kind == RequestFreeform {
		want, err := NewFreeformRequest(request.ID, request.ToolCallID, request.Prompt)
		if err != nil {
			return err
		}

		if len(request.Questions) != 0 || want.SchemaDigest != request.SchemaDigest {
			return fmt.Errorf("%w: schema digest mismatch", ErrInvalid)
		}

		return nil
	}

	if request.Kind != RequestStructured || request.Prompt != "" {
		return fmt.Errorf("%w: unsupported request kind", ErrInvalid)
	}

	digest, err := Digest(Spec{Questions: request.Questions})
	if err != nil {
		return err
	}

	if digest != request.SchemaDigest {
		return fmt.Errorf("%w: schema digest mismatch", ErrInvalid)
	}

	return nil
}

// ValidateResolution verifies one exact response against its pending request.
func ValidateResolution(request Request, resolution Resolution) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}

	if resolution.RequestID != request.ID || resolution.SchemaDigest != request.SchemaDigest {
		return fmt.Errorf("%w: resolution does not match request", ErrInvalid)
	}

	if resolution.Chat != "" {
		if !validText(resolution.Chat, maxChatBytes) || len(resolution.Answers) != 0 {
			return fmt.Errorf("%w: chat response is malformed", ErrInvalid)
		}

		return nil
	}

	if request.Kind == RequestFreeform {
		return fmt.Errorf("%w: free-form response is required", ErrInvalid)
	}

	if len(resolution.Answers) != len(request.Questions) {
		return fmt.Errorf("%w: every question requires an answer", ErrInvalid)
	}

	for index, answer := range resolution.Answers {
		if err := validateAnswer(request.Questions[index], answer); err != nil {
			return fmt.Errorf("%w: answer %d: %w", ErrInvalid, index+1, err)
		}
	}

	return nil
}

// ValidateResolutionShape validates bounded content that does not require the
// pending request schema. Runtime still calls ValidateResolution before use.
//
//nolint:gocyclo // Mutually exclusive answer forms require explicit shape checks.
func ValidateResolutionShape(resolution Resolution) error {
	if !validIdentity(resolution.RequestID) || len(resolution.SchemaDigest) != sha256.Size*2 {
		return fmt.Errorf("%w: invalid resolution identity", ErrInvalid)
	}

	if _, err := hex.DecodeString(resolution.SchemaDigest); err != nil {
		return fmt.Errorf("%w: invalid schema digest", ErrInvalid)
	}

	if resolution.Chat != "" {
		if !validText(resolution.Chat, maxChatBytes) || len(resolution.Answers) != 0 {
			return fmt.Errorf("%w: chat response is malformed", ErrInvalid)
		}

		return nil
	}

	if len(resolution.Answers) < 1 || len(resolution.Answers) > 4 {
		return fmt.Errorf("%w: invalid answer count", ErrInvalid)
	}

	for _, answer := range resolution.Answers {
		if answer.Custom != "" {
			if !validText(answer.Custom, maxCustomBytes) || len(answer.Selections) != 0 {
				return fmt.Errorf("%w: malformed custom answer", ErrInvalid)
			}

			continue
		}

		if len(answer.Selections) < 1 || len(answer.Selections) > 4 {
			return fmt.Errorf("%w: malformed selections", ErrInvalid)
		}

		for _, selection := range answer.Selections {
			if !validText(selection, maxLabelBytes) {
				return fmt.Errorf("%w: malformed selection", ErrInvalid)
			}
		}
	}

	return nil
}

// CloneRequest returns a fully detached Request.
func CloneRequest(request Request) Request {
	request.Questions = cloneQuestions(request.Questions)

	return request
}

// CloneResolution returns a fully detached Resolution.
func CloneResolution(resolution Resolution) Resolution {
	resolution.Answers = slices.Clone(resolution.Answers)
	for index := range resolution.Answers {
		resolution.Answers[index].Selections = slices.Clone(resolution.Answers[index].Selections)
	}

	return resolution
}

// RequestCount returns the number of user-visible decisions. A free-form
// fallback is one decision even though it has no structured options.
func RequestCount(request Request) int {
	if request.Kind == RequestFreeform {
		return 1
	}

	return len(request.Questions)
}

func validateAnswer(item Question, answer Answer) error {
	if answer.Custom != "" {
		if !validText(answer.Custom, maxCustomBytes) || len(answer.Selections) != 0 {
			return errors.New("custom answer is malformed")
		}

		return nil
	}

	if len(answer.Selections) == 0 || (!item.Multiple && len(answer.Selections) != 1) {
		return errors.New("selection count is invalid")
	}

	lastIndex := -1

	for _, selected := range answer.Selections {
		optionIndex := slices.IndexFunc(item.Options, func(option Option) bool {
			return option.Label == selected
		})
		if optionIndex < 0 || optionIndex <= lastIndex {
			return errors.New("selections are unknown, duplicated, or out of order")
		}

		lastIndex = optionIndex
	}

	return nil
}

func cloneQuestions(questions []Question) []Question {
	cloned := slices.Clone(questions)
	for index := range cloned {
		cloned[index].Options = slices.Clone(cloned[index].Options)
	}

	return cloned
}

func validIdentity(value string) bool {
	return validText(value, 512) && !strings.ContainsAny(value, "\r\n\x00")
}

func validText(value string, limit int) bool {
	return value != "" && validOptionalText(value, limit)
}

func validOptionalText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}
