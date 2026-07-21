package ai

import "github.com/rsbin/pips/ai/internal/jsonx"

// ValidateRequestBodyExtension validates a bounded raw JSON object against
// reserved dotted paths without sending a request. Provider adapters enforce
// the same rules again while merging into their typed wire body.
func ValidateRequestBodyExtension(extra map[string]any, reserved ...string) error {
	_, err := jsonx.MergeExtraFields(struct{}{}, extra, reserved...)

	return err
}
