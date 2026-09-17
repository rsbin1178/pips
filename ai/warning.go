package ai

// WarningType classifies a notice about how a request was encoded.
type WarningType string

// Warning types, mirroring the structured-warning contract of the wider
// ecosystem (for example the Vercel AI SDK): a warning is data the caller can
// read, not a log line.
const (
	// WarningUnsupported means the provider or protocol does not support what
	// was requested, so it was not sent.
	WarningUnsupported WarningType = "unsupported"
	// WarningCompatibility means the value was sent, but encoded differently
	// from what was requested to fit the endpoint.
	WarningCompatibility WarningType = "compatibility"
	// WarningDeprecated means the requested field is deprecated in favor of
	// another one.
	WarningDeprecated WarningType = "deprecated"
)

// Warning is one structured notice about a request whose configuration did not
// reach the wire exactly as written. Adapters emit it instead of silently
// dropping a configured field, so a caller can tell whether its request took
// effect: an empty Warnings slice means everything asked for was sent as asked.
type Warning struct {
	// Type classifies the notice.
	Type WarningType
	// Feature names the field or capability the notice is about, for example
	// "reasoning_effort", "response_format", or a provider tool name.
	Feature string
	// Details explains what happened and why, in one sentence.
	Details string
}
