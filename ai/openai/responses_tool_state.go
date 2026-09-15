package openai

import "strings"

// Tool-call item ids.
//
// A Responses function_call identifies the call twice: by item id (fc_...) and
// by call id (call_...). The portable model keys tool results by a single id,
// so the adapter keeps the call id as the portable tool-call id and appends the
// provider's item id to it:
//
//	call_<callID>|id=<itemID>
//
// Replaying the assistant message therefore restores both fields. Input items
// are validated item by item, and a function_call without its item id is
// rejected, even though openai.com accepts one.
const toolCallIDSeparator = "|id="

// encodeResponsesToolCallID merges a wire call id and item id into the portable
// tool-call id. A call whose item id is absent keeps the bare call id.
func encodeResponsesToolCallID(callID, itemID string) string {
	if itemID == "" {
		return callID
	}

	return callID + toolCallIDSeparator + itemID
}

// decodeResponsesToolCallID splits a portable tool-call id back into the wire
// call id and the provider's item id. An id that carries no item id is the call
// id itself.
func decodeResponsesToolCallID(id string) (callID, itemID string) {
	callID, itemID, ok := strings.Cut(id, toolCallIDSeparator)
	if !ok {
		return id, ""
	}

	return callID, itemID
}

// functionCallItemID returns the item id to replay for a tool call. The field
// carries provider-assigned identity and is required by strict servers, so an
// id is derived from the call id when the response supplied none — history
// replayed from another provider, or assistant messages built by hand.
func functionCallItemID(callID, itemID string) string {
	if itemID != "" {
		return itemID
	}

	return "fc_" + callID
}
