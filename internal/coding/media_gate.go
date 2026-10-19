package coding

import (
	"context"
	"strings"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

// Media kinds the gate names in a refusal. They are also the words the model
// sees, so they stay lowercase and human-readable.
const (
	mediaKindImage = "image"
	mediaKindFile  = "file"
)

// MediaCapabilityGate refuses tool-result media the active model cannot accept.
// It mirrors the model's declared capabilities: image parts need Vision and
// file parts need Documents. Text, structured content, and resource links pass
// through unchanged. Embedded resources are judged by their blob, because
// [ai.ProviderParts] projects an embedded blob into an image or file part
// before it reaches a provider.
type MediaCapabilityGate struct {
	// Vision reports whether the model accepts image input.
	Vision bool
	// Documents reports whether the model accepts document input.
	Documents bool
}

// MediaGateForCapabilities builds the gate for a model's declared capabilities.
func MediaGateForCapabilities(capabilities ai.Capabilities) MediaCapabilityGate {
	return MediaCapabilityGate{Vision: capabilities.Vision, Documents: capabilities.Documents}
}

// Check rewrites every media part the gate refuses into a text result naming
// the missing capability. The bool reports whether anything was refused, so a
// caller can leave an accepted result untouched.
func (g MediaCapabilityGate) Check(toolName string, parts []ai.Part) ([]ai.Part, bool) {
	out := make([]ai.Part, 0, len(parts))
	refused := false

	for _, part := range parts {
		kind, refuse := g.refusedMediaKind(part)
		if !refuse {
			out = append(out, part)

			continue
		}

		out = append(out, ai.TextPart{Text: mediaRefusalText(kind, toolName)})
		refused = true
	}

	return out, refused
}

// refusedMediaKind names the media kind the gate refuses, or "" when the part
// is acceptable.
func (g MediaCapabilityGate) refusedMediaKind(part ai.Part) (string, bool) {
	switch value := part.(type) {
	case ai.ImagePart:
		return mediaKindImage, !g.Vision
	case ai.FilePart:
		return mediaKindFile, !g.Documents
	case ai.EmbeddedResourcePart:
		if value.Blob == nil {
			return "", false
		}

		if strings.HasPrefix(strings.ToLower(value.MIMEType), "image/") {
			return mediaKindImage, !g.Vision
		}

		return mediaKindFile, !g.Documents
	default:
		return "", false
	}
}

// mediaRefusalText names the missing capability and the remedy, in the same
// style as the Composer's vision refusal.
func mediaRefusalText(kind, toolName string) string {
	if kind == mediaKindImage {
		return "the active model does not accept image input: " + toolName +
			" returned an image; declare capabilities.vision = true for this model"
	}

	return "the active model does not accept document input: " + toolName +
		" returned a file; declare capabilities.documents = true for this model"
}

// mediaGateAfterTool applies the gate at the tool-result boundary, so a refusal
// is what gets committed to the transcript and sent to the provider. The gate
// reads the model's capabilities per call, so a model switch is reflected in
// later results. A result with nothing to refuse is left untouched.
func mediaGateAfterTool(model ai.LanguageModel) func(context.Context, agent.ToolResultInfo) *agent.ToolResultOverride {
	return func(_ context.Context, info agent.ToolResultInfo) *agent.ToolResultOverride {
		if model == nil {
			return nil
		}

		gated, refused := MediaGateForCapabilities(model.Capabilities()).Check(info.Name, info.Result.Content)
		if !refused {
			return nil
		}

		return &agent.ToolResultOverride{Content: gated}
	}
}
