package agent

import (
	"bytes"
	"encoding/json"
	"slices"
	"unicode/utf8"

	"github.com/rsbin1178/pips/ai"
)

// undecodableArgumentsField marks tool arguments that the model emitted as
// malformed JSON. The wrapper object keeps the transcript persistable and keeps
// the original text next to the call; [funcTool.Exec] refuses to decode a call
// carrying it, so a tool never runs with silently defaulted fields.
const undecodableArgumentsField = "pips_undecodable_arguments"

// maxArgumentsEchoBytes bounds the original text echoed back to the model. The
// model already holds its own output in context, so a prefix plus the decode
// diagnosis is enough.
const maxArgumentsEchoBytes = 2_000

// normalizeModelToolArguments replaces tool arguments that are not well-formed
// JSON with a wrapper carrying the original text. A response cut short by the
// output limit leaves a truncated object, and the session store marshals
// arguments verbatim: invalid raw JSON would drop the whole message.
func normalizeModelToolArguments(message ai.AssistantMessage) ai.AssistantMessage {
	var normalized []ai.AssistantPart

	for index, part := range message.Parts {
		call, ok := part.(ai.ToolCallPart)
		if !ok {
			continue
		}

		arguments := bytes.TrimSpace(call.Args)
		if len(arguments) == 0 || json.Valid(arguments) {
			continue
		}

		if normalized == nil {
			normalized = slices.Clone(message.Parts)
		}

		normalized[index] = ai.ToolCallPart{
			ID:   call.ID,
			Name: call.Name,
			Args: undecodableToolArguments(arguments),
		}
	}

	if normalized == nil {
		return message
	}

	return ai.AssistantMessage{Parts: normalized}
}

// undecodableToolArguments wraps arguments that are not usable JSON in an object
// that stays decodable for storage and display.
func undecodableToolArguments(arguments []byte) ai.JSON {
	if len(arguments) > maxArgumentsEchoBytes {
		arguments = arguments[:maxArgumentsEchoBytes]
		for len(arguments) > 0 && !utf8.Valid(arguments) {
			arguments = arguments[:len(arguments)-1]
		}
	}

	encoded, err := json.Marshal(map[string]string{undecodableArgumentsField: string(arguments)})
	if err != nil {
		return ai.JSON(`{"` + undecodableArgumentsField + `":""}`)
	}

	return ai.JSON(encoded)
}

// modelToolArgumentsUndecodable reports the original arguments text when a tool
// call carries the malformed-JSON marker.
func modelToolArgumentsUndecodable(arguments ai.JSON) (string, bool) {
	var wrapper map[string]string

	if err := json.Unmarshal(arguments, &wrapper); err != nil {
		return "", false
	}

	original, ok := wrapper[undecodableArgumentsField]

	return original, ok
}
