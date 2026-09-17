package ai

// CapabilityOverride is a field-level, tri-state overlay on [Capabilities].
// A nil field means "inherit"; a non-nil field is an explicit declaration, so
// an explicit false stays distinguishable from "not declared".
//
// It mirrors every [Capabilities] field so a capability added to the base type
// becomes visible here.
type CapabilityOverride struct {
	// Text reports whether the model generates text.
	Text *bool
	// Vision reports whether the model accepts image input.
	Vision *bool
	// Documents reports whether the model accepts document/file input.
	Documents *bool
	// AudioInput reports whether the model accepts audio input.
	AudioInput *bool
	// VideoInput reports whether the model accepts video input.
	VideoInput *bool
	// Tools reports whether the model supports tool / function calling.
	Tools *bool
	// StructuredOutput reports whether the model supports schema-constrained
	// JSON output.
	StructuredOutput *bool
	// Reasoning reports whether the model exposes reasoning controls/content.
	Reasoning *bool
	// ImageGeneration reports whether the model can generate images.
	ImageGeneration *bool
	// Embeddings reports whether the model produces embeddings.
	Embeddings *bool
	// Reranking reports whether the model performs document reranking.
	Reranking *bool
	// PromptCaching reports whether the provider supports prompt-cache reuse
	// or cache-usage reporting for this model.
	PromptCaching *bool
	// TokenCounting reports whether the model exposes server-side token
	// counting without running inference.
	TokenCounting *bool
	// WebSearch reports whether the model supports provider-executed web
	// search.
	WebSearch *bool
	// CodeExecution reports whether the model supports server-side code
	// execution.
	CodeExecution *bool
}

// IsZero reports whether no field is declared.
func (o CapabilityOverride) IsZero() bool {
	return o == CapabilityOverride{}
}

// CapabilityDeclaration is one declared capability field.
type CapabilityDeclaration struct {
	// Name is the stable field name, for example "vision" or "audio_input".
	Name string
	// Value is the declared value.
	Value bool
}

// Declarations returns the declared fields in a stable order. It lets callers
// report exactly what was declared without repeating the field list.
func (o CapabilityOverride) Declarations() []CapabilityDeclaration {
	fields := []struct {
		name  string
		value *bool
	}{
		{"text", o.Text},
		{"vision", o.Vision},
		{"documents", o.Documents},
		{"audio_input", o.AudioInput},
		{"video_input", o.VideoInput},
		{"tools", o.Tools},
		{"structured_output", o.StructuredOutput},
		{"reasoning", o.Reasoning},
		{"image_generation", o.ImageGeneration},
		{"embeddings", o.Embeddings},
		{"reranking", o.Reranking},
		{"prompt_caching", o.PromptCaching},
		{"token_counting", o.TokenCounting},
		{"web_search", o.WebSearch},
		{"code_execution", o.CodeExecution},
	}

	declarations := make([]CapabilityDeclaration, 0, len(fields))
	for _, field := range fields {
		if field.value != nil {
			declarations = append(declarations, CapabilityDeclaration{Name: field.name, Value: *field.value})
		}
	}

	return declarations
}

// Apply returns base with every declared field replaced by its declaration.
// Undeclared fields keep the base value.
//
//nolint:dupl // Apply and Overlay are the same field mapping in opposite directions; both stay explicit to mirror the struct.
func (o CapabilityOverride) Apply(base Capabilities) Capabilities {
	applyCapability(&base.Text, o.Text)
	applyCapability(&base.Vision, o.Vision)
	applyCapability(&base.Documents, o.Documents)
	applyCapability(&base.AudioInput, o.AudioInput)
	applyCapability(&base.VideoInput, o.VideoInput)
	applyCapability(&base.Tools, o.Tools)
	applyCapability(&base.StructuredOutput, o.StructuredOutput)
	applyCapability(&base.Reasoning, o.Reasoning)
	applyCapability(&base.ImageGeneration, o.ImageGeneration)
	applyCapability(&base.Embeddings, o.Embeddings)
	applyCapability(&base.Reranking, o.Reranking)
	applyCapability(&base.PromptCaching, o.PromptCaching)
	applyCapability(&base.TokenCounting, o.TokenCounting)
	applyCapability(&base.WebSearch, o.WebSearch)
	applyCapability(&base.CodeExecution, o.CodeExecution)

	return base
}

func applyCapability(target, value *bool) {
	if value != nil {
		*target = *value
	}
}

// Overlay returns o with every field declared in child taking precedence.
// The receiver is the base layer and child wins, matching the layering used by
// the coding configuration.
//
//nolint:dupl // Apply and Overlay are the same field mapping in opposite directions; both stay explicit to mirror the struct.
func (o CapabilityOverride) Overlay(child CapabilityOverride) CapabilityOverride {
	overlayCapability(&o.Text, child.Text)
	overlayCapability(&o.Vision, child.Vision)
	overlayCapability(&o.Documents, child.Documents)
	overlayCapability(&o.AudioInput, child.AudioInput)
	overlayCapability(&o.VideoInput, child.VideoInput)
	overlayCapability(&o.Tools, child.Tools)
	overlayCapability(&o.StructuredOutput, child.StructuredOutput)
	overlayCapability(&o.Reasoning, child.Reasoning)
	overlayCapability(&o.ImageGeneration, child.ImageGeneration)
	overlayCapability(&o.Embeddings, child.Embeddings)
	overlayCapability(&o.Reranking, child.Reranking)
	overlayCapability(&o.PromptCaching, child.PromptCaching)
	overlayCapability(&o.TokenCounting, child.TokenCounting)
	overlayCapability(&o.WebSearch, child.WebSearch)
	overlayCapability(&o.CodeExecution, child.CodeExecution)

	return o
}

func overlayCapability(target **bool, value *bool) {
	if value != nil {
		*target = value
	}
}

// Clone returns a detached copy sharing no pointer with o.
func (o CapabilityOverride) Clone() CapabilityOverride {
	return CapabilityOverride{
		Text:             cloneCapabilityBool(o.Text),
		Vision:           cloneCapabilityBool(o.Vision),
		Documents:        cloneCapabilityBool(o.Documents),
		AudioInput:       cloneCapabilityBool(o.AudioInput),
		VideoInput:       cloneCapabilityBool(o.VideoInput),
		Tools:            cloneCapabilityBool(o.Tools),
		StructuredOutput: cloneCapabilityBool(o.StructuredOutput),
		Reasoning:        cloneCapabilityBool(o.Reasoning),
		ImageGeneration:  cloneCapabilityBool(o.ImageGeneration),
		Embeddings:       cloneCapabilityBool(o.Embeddings),
		Reranking:        cloneCapabilityBool(o.Reranking),
		PromptCaching:    cloneCapabilityBool(o.PromptCaching),
		TokenCounting:    cloneCapabilityBool(o.TokenCounting),
		WebSearch:        cloneCapabilityBool(o.WebSearch),
		CodeExecution:    cloneCapabilityBool(o.CodeExecution),
	}
}

func cloneCapabilityBool(value *bool) *bool {
	if value == nil {
		return nil
	}

	return new(*value)
}
