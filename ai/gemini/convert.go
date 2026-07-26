//nolint:wsl_v5 // Wire translation keeps presence-aware assignments adjacent.
package gemini

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/internal/jsonx"
)

// RequestOptions is the gemini entry for [ai.Request.ProviderOptions].
type RequestOptions struct {
	// CachedContent references an explicit Gemini cache resource, for example
	// "cachedContents/abc123".
	CachedContent string
	// ExtraFields is merged into the outgoing JSON request using bounded
	// recursive add-only semantics. It is the escape hatch for non-reserved
	// parameters not modeled portably (for example safetySettings).
	ExtraFields map[string]any
}

func requestOptions(req ai.Request, provider ai.Provider) RequestOptions {
	if raw, ok := req.ProviderOptions[provider]; ok {
		if opts, ok := raw.(RequestOptions); ok {
			return opts
		}
	}
	if provider != ai.ProviderGemini {
		if raw, ok := req.ProviderOptions[ai.ProviderGemini]; ok {
			if opts, ok := raw.(RequestOptions); ok {
				return opts
			}
		}
	}

	return RequestOptions{}
}

// requestFrom translates a portable request into the generateContent wire
// shape.
func requestFrom(req ai.Request, provider ai.Provider) (any, error) {
	if err := validateGenerationControls(req); err != nil {
		return nil, err
	}
	if req.Reasoning != nil && req.Reasoning.Mode == ai.ReasoningModeAdaptive {
		return nil, fmt.Errorf("gemini: adaptive reasoning is unsupported: %w", ai.ErrUnsupported)
	}
	opts := requestOptions(req, provider)

	contents, err := contentsFrom(req.Messages)
	if err != nil {
		return nil, err
	}

	out := generateRequest{
		Contents:          contents,
		SystemInstruction: systemInstructionFrom(req.System),
		CachedContent:     opts.CachedContent,
		Tools:             toolsFrom(req.Tools),
		ToolConfig:        toolConfigFrom(req.ToolChoice),
		GenerationConfig:  generationConfigFrom(req),
	}

	return mergeExtraFields(out, opts.ExtraFields)
}

func validateGenerationControls(req ai.Request) error {
	rules := []struct {
		invalid bool
		message string
	}{
		{outside(req.Temperature, 0, 2), "temperature must be within 0..2"},
		{outside(req.TopP, 0, 1), "top-p must be within 0..1"},
		{outsidePositiveInt32(req.TopK), fmt.Sprintf("top-k must be within 1..%d", math.MaxInt32)},
		{outsideSignedInt32(req.Seed), "seed must fit int32"},
		{outside(req.FrequencyPenalty, -2, 2), "frequency penalty must be within -2..2"},
		{outside(req.PresencePenalty, -2, 2), "presence penalty must be within -2..2"},
		{outsidePositiveInt32(req.MaxTokens), fmt.Sprintf("max tokens must be within 1..%d", math.MaxInt32)},
		{invalidLogProbs(req.LogProbs), "top logprobs must be within 0..20"},
		{invalidThinkingBudget(req.Reasoning), fmt.Sprintf("thinking budget must be within 0..%d", math.MaxInt32)},
	}
	for _, rule := range rules {
		if rule.invalid {
			return fmt.Errorf("gemini: %s: %w", rule.message, ai.ErrInvalidRequest)
		}
	}
	if req.Reasoning != nil && !validThinkingLevel(req.Reasoning) {
		return fmt.Errorf("gemini: unsupported thinking level %q: %w", req.Reasoning.Effort, ai.ErrUnsupported)
	}

	return nil
}

func validThinkingLevel(value *ai.ReasoningConfig) bool {
	if value.Mode == ai.ReasoningModeDisabled {
		return true
	}

	switch value.Effort {
	case "", ai.ReasoningNone, ai.ReasoningMinimal, ai.ReasoningLow,
		ai.ReasoningMedium, ai.ReasoningHigh:
		return true
	default:
		return value.BudgetTokens != 0
	}
}

func outsidePositiveInt32(value *int) bool {
	return value != nil && (*value <= 0 || int64(*value) > math.MaxInt32)
}

func outsideSignedInt32(value *int64) bool {
	return value != nil && (*value < math.MinInt32 || *value > math.MaxInt32)
}

func invalidLogProbs(value *ai.LogProbsConfig) bool {
	return value != nil && (value.Top < 0 || value.Top > 20)
}

func invalidThinkingBudget(value *ai.ReasoningConfig) bool {
	return value != nil && (value.BudgetTokens < 0 || int64(value.BudgetTokens) > math.MaxInt32)
}

func outside(value *float64, minimum, maximum float64) bool {
	return value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) ||
		*value < minimum || *value > maximum)
}

func systemInstructionFrom(system string) *wireContent {
	if system == "" {
		return nil
	}

	return &wireContent{Parts: []wirePart{{Text: system}}}
}

// contentsFrom converts the conversation. Gemini has only "user" and "model"
// roles; system prompts move to systemInstruction and tool results become
// user turns carrying functionResponse parts.
func contentsFrom(msgs []ai.Message) ([]wireContent, error) {
	out := make([]wireContent, 0, len(msgs))

	for _, msg := range msgs {
		content, err := contentFrom(msg)
		if err != nil {
			return nil, err
		}

		if content != nil {
			out = append(out, *content)
		}
	}

	return out, nil
}

func contentFrom(msg ai.Message) (*wireContent, error) {
	switch msg.Role {
	case ai.RoleUser:
		parts, err := userPartsFrom(msg.Parts)
		if err != nil {
			return nil, err
		}

		return &wireContent{Role: roleUser, Parts: parts}, nil
	case ai.RoleAssistant:
		parts, err := modelPartsFrom(msg.Parts)
		if err != nil {
			return nil, err
		}

		return &wireContent{Role: roleModel, Parts: parts}, nil
	case ai.RoleTool:
		parts, err := functionResponseParts(msg.Parts)
		if err != nil {
			return nil, err
		}

		return &wireContent{Role: roleUser, Parts: parts}, nil
	case ai.RoleSystem:
		return &wireContent{Role: roleUser, Parts: []wirePart{{Text: textOf(msg.Parts)}}}, nil
	default:
		return nil, fmt.Errorf("gemini: unsupported message role %q", msg.Role)
	}
}

func userPartsFrom(parts []ai.Part) ([]wirePart, error) {
	out := make([]wirePart, 0, len(parts))

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			out = append(out, wirePart{Text: p.Text})
		case ai.ImagePart:
			if p.Source.IsID() {
				return nil, fmt.Errorf("gemini: image parts require inline data or a file URI, got provider ID %q: %w", p.Source.ID, ai.ErrUnsupported)
			}

			out = append(out, mediaPartFrom(p.Source))
		case ai.FilePart:
			if p.Source.IsID() {
				return nil, fmt.Errorf("gemini: file parts require inline data or a file URI, got provider ID %q: %w", p.Source.ID, ai.ErrUnsupported)
			}

			out = append(out, mediaPartFrom(p.Source))
		default:
			return nil, fmt.Errorf("gemini: part %T not supported in user messages", part)
		}
	}

	return out, nil
}

func modelPartsFrom(parts []ai.Part) ([]wirePart, error) {
	out := make([]wirePart, 0, len(parts))

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			out = append(out, wirePart{Text: p.Text})
		case ai.ReasoningPart:
			// Echo thinking back as a thought part carrying its signature.
			out = append(out, wirePart{Text: p.Text, Thought: true, ThoughtSignature: p.Signature})
		case ai.ToolCallPart:
			out = append(out, wirePart{
				FunctionCall:     &wireFunctionCall{ID: originalCallID(p.ID), Name: p.Name, Args: p.Args},
				ThoughtSignature: signatureFromID(p.ID),
			})
		default:
			return nil, fmt.Errorf("gemini: part %T not supported in assistant messages", part)
		}
	}

	return out, nil
}

// functionResponseParts converts tool results. Gemini keys a response by the
// function name (and, for Gemini 3, the original call id), not a tool_call_id,
// so the synthesized id is decoded back to name+id here.
func functionResponseParts(parts []ai.Part) ([]wirePart, error) {
	out := make([]wirePart, 0, len(parts))

	for _, part := range parts {
		result, ok := part.(ai.ToolResultPart)
		if !ok {
			return nil, fmt.Errorf("gemini: tool messages may only contain tool results, got %T", part)
		}

		out = append(out, wirePart{FunctionResponse: &wireFunctionResp{
			ID:       originalCallID(result.ToolCallID),
			Name:     result.Name,
			Response: functionResponseValue(result),
		}})
	}

	return out, nil
}

// functionResponseValue wraps tool output in the object Gemini expects.
// Structured JSON output is passed through; plain text is wrapped under
// "output"; errors under "error".
func functionResponseValue(result ai.ToolResultPart) map[string]any {
	text := textOf(result.Content)

	key := "output"
	if result.IsError {
		key = "error"
	}

	var parsed map[string]any
	if err := jsonx.Unmarshal([]byte(text), &parsed); err == nil {
		return parsed
	}

	return map[string]any{key: text}
}

func mediaPartFrom(src ai.MediaSource) wirePart {
	if src.IsURL() {
		return wirePart{FileData: &wireFileData{MIMEType: src.MIMEType, FileURI: src.URL}}
	}

	return wirePart{InlineData: &wireBlob{MIMEType: src.MIMEType, Data: base64.StdEncoding.EncodeToString(src.Data)}}
}

func toolsFrom(tools []ai.Tool) []wireTool {
	if len(tools) == 0 {
		return nil
	}

	decls := make([]wireFunctionDecl, 0, len(tools))
	for _, tool := range tools {
		decls = append(decls, wireFunctionDecl{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.EffectiveInputSchema(),
		})
	}

	return []wireTool{{FunctionDeclarations: decls}}
}

func toolConfigFrom(choice ai.ToolChoice) *wireToolConfig {
	mode := ""

	switch choice.Mode {
	case ai.ToolChoiceAuto:
		mode = "AUTO"
	case ai.ToolChoiceRequired, ai.ToolChoiceTool:
		mode = "ANY"
	case ai.ToolChoiceNone:
		mode = "NONE"
	default:
		return nil
	}

	cfg := &wireFunctionCallingConfig{Mode: mode}
	if choice.Mode == ai.ToolChoiceTool {
		cfg.AllowedFunctionNames = []string{choice.Name}
	}

	return &wireToolConfig{FunctionCallingConfig: cfg}
}

func generationConfigFrom(req ai.Request) *generationConfig {
	cfg := &generationConfig{
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		TopK:             req.TopK,
		Seed:             req.Seed,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		MaxOutputTokens:  req.MaxTokens,
		StopSequences:    req.Stop,
	}
	if req.LogProbs != nil {
		cfg.ResponseLogProbs = &req.LogProbs.Enabled
		if req.LogProbs.Top != 0 {
			cfg.LogProbs = &req.LogProbs.Top
		}
	}

	if rf := req.ResponseFormat; rf != nil && rf.Schema != nil {
		cfg.ResponseMIMEType = "application/json"
		cfg.ResponseSchema = rf.Schema
	}

	if tc := thinkingConfigFrom(req.Reasoning); tc != nil {
		cfg.ThinkingConfig = tc
	}

	if isEmptyGenerationConfig(cfg) {
		return nil
	}

	return cfg
}

func thinkingConfigFrom(r *ai.ReasoningConfig) *thinkingConfig {
	if r == nil {
		return nil
	}
	if r.Mode == ai.ReasoningModeDisabled || r.Effort == ai.ReasoningNone {
		budget := 0
		return &thinkingConfig{ThinkingBudget: &budget, IncludeThoughts: r.IncludeSummary}
	}

	budget := r.BudgetTokens
	if budget == 0 && r.Effort == "" && !r.IncludeSummary {
		return nil
	}

	tc := &thinkingConfig{IncludeThoughts: r.IncludeSummary}
	if r.Effort != "" && r.BudgetTokens == 0 {
		tc.ThinkingLevel = strings.ToUpper(string(r.Effort))
	}
	if budget != 0 {
		tc.ThinkingBudget = &budget
	}

	return tc
}

func isEmptyGenerationConfig(cfg *generationConfig) bool {
	return cfg.Temperature == nil && cfg.TopP == nil && cfg.TopK == nil && cfg.Seed == nil &&
		cfg.FrequencyPenalty == nil && cfg.PresencePenalty == nil &&
		cfg.ResponseLogProbs == nil && cfg.LogProbs == nil && cfg.MaxOutputTokens == nil &&
		len(cfg.StopSequences) == 0 && cfg.ResponseMIMEType == "" &&
		cfg.ResponseSchema == nil && cfg.ThinkingConfig == nil
}

// Synthetic tool-call IDs.
//
// Gemini 2.5 and earlier return functionCall parts with no id, but the
// portable model keys tool results by id. The adapter assigns each call a
// stable synthetic id and encodes the provider's real id and thought
// signature (Gemini 3) so both round-trip on the functionResponse. Format:
//
//	call_<index>[|id=<realID>][|sig=<b64Signature>]
//
// A response whose id was synthesized here carries no real id; originalCallID
// then returns "" and the functionResponse is matched by name, which the API
// accepts for the models that omit ids.
const syntheticIDPrefix = "call_"

func syntheticCallID(index int, realID, signature string) string {
	var b strings.Builder

	b.WriteString(syntheticIDPrefix)
	b.WriteString(strconv.Itoa(index))

	if realID != "" {
		b.WriteString("|id=")
		b.WriteString(realID)
	}

	if signature != "" {
		b.WriteString("|sig=")
		b.WriteString(signature)
	}

	return b.String()
}

// originalCallID recovers the provider's real call id from a synthetic id, or
// "" when the id was synthesized locally (the model supplied none).
func originalCallID(id string) string {
	realID, _ := splitSyntheticID(id)
	return realID
}

func signatureFromID(id string) string {
	_, sig := splitSyntheticID(id)
	return sig
}

func splitSyntheticID(id string) (realID, signature string) {
	for field := range strings.SplitSeq(id, "|") {
		switch {
		case strings.HasPrefix(field, "id="):
			realID = strings.TrimPrefix(field, "id=")
		case strings.HasPrefix(field, "sig="):
			signature = strings.TrimPrefix(field, "sig=")
		}
	}

	return realID, signature
}
