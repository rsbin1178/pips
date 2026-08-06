//nolint:wsl_v5 // Wire validation keeps the semantic checks together.
package pluginv1

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/jsonx"
)

const (
	// ApplicationProtocolMajor identifies the MVP domain protocol major.
	ApplicationProtocolMajor uint32 = 1
	// ApplicationProtocolMinor identifies the MVP domain protocol minor.
	ApplicationProtocolMinor uint32 = 0

	// ToolsCapabilityName identifies the tools.v1 capability family.
	ToolsCapabilityName = "tools.v1"
	// ToolsCapabilityMajor identifies the tools.v1 capability major.
	ToolsCapabilityMajor uint32 = 1

	// maxIdentifierBytes bounds protocol identifiers.
	maxIdentifierBytes     = 128
	maxDescriptionBytes    = 16 << 10
	maxJSONSchemaBytes     = 256 << 10
	maxToolContentBytes    = 1 << 20
	maxToolResultBytes     = 8 << 20
	maxEventMessageBytes   = 16 << 10
	maxFailureProblems     = 32
	maxCapabilityCount     = 64
	maxStateSchemaCount    = 64
	maxToolCount           = 4096
	maxProgressMetadata    = 64 << 10
	maxFailureDetailsBytes = 64 << 10
)

// ErrInvalid reports a semantic protocol validation failure.
var ErrInvalid = errors.New("pluginv1: invalid protocol value")

// ValidationError identifies a semantic field without copying untrusted
// payloads into an error message.
type ValidationError struct {
	Path   string
	Reason string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ErrInvalid.Error()
	}
	return fmt.Sprintf("pluginv1: invalid %s (%s)", e.Path, e.Reason)
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

func invalid(path, reason string) error {
	return &ValidationError{Path: path, Reason: reason}
}

// ValidatePluginDescription validates the immutable declarations returned by
// CoreService.Describe.
//
//nolint:gocyclo // Description validation keeps all publication invariants together.
func ValidatePluginDescription(description *DescribeResponse) error {
	if description == nil {
		return invalid("description", "missing")
	}
	if err := validateIdentity(description.GetIdentity()); err != nil {
		return err
	}
	if err := validateProtocolRange("application_protocol", description.GetApplicationProtocol()); err != nil {
		return err
	}
	if len(description.GetCapabilities()) > maxCapabilityCount {
		return invalid("capabilities", "too many")
	}
	seenCapabilities := make(map[string]struct{}, len(description.GetCapabilities()))
	for i, capability := range description.GetCapabilities() {
		path := fmt.Sprintf("capabilities[%d]", i)
		if err := ValidateCapability(path, capability); err != nil {
			return err
		}
		if _, ok := seenCapabilities[capability.GetName()]; ok {
			return invalid(path+".name", "duplicate")
		}
		seenCapabilities[capability.GetName()] = struct{}{}
	}
	if len(description.GetStateSchemas()) > maxStateSchemaCount {
		return invalid("state_schemas", "too many")
	}
	seenState := make(map[string]struct{}, len(description.GetStateSchemas()))
	for i, schema := range description.GetStateSchemas() {
		path := fmt.Sprintf("state_schemas[%d]", i)
		if schema == nil || schema.GetKind() == "" {
			return invalid(path, "missing kind")
		}
		if err := validateIdentifier(path+".kind", schema.GetKind()); err != nil {
			return err
		}
		if schema.GetVersion() == 0 {
			return invalid(path+".version", "must be positive")
		}
		if _, ok := seenState[schema.GetKind()]; ok {
			return invalid(path+".kind", "duplicate")
		}
		seenState[schema.GetKind()] = struct{}{}
	}
	return ValidateResourceLimits(description.GetLimits())
}

func validateIdentity(identity *PluginIdentity) error {
	if identity == nil {
		return invalid("identity", "missing")
	}
	if err := validateIdentifier("identity.id", identity.GetId()); err != nil {
		return err
	}
	if err := validateText("identity.semantic_version", identity.GetSemanticVersion(), maxIdentifierBytes, false); err != nil {
		return err
	}
	if identity.GetDisplayName() != "" {
		if err := validateText("identity.display_name", identity.GetDisplayName(), maxDescriptionBytes, true); err != nil {
			return err
		}
	}
	if identity.GetDescription() != "" {
		if err := validateText("identity.description", identity.GetDescription(), maxDescriptionBytes, true); err != nil {
			return err
		}
	}
	return nil
}

func validateProtocolRange(path string, version *ProtocolRange) error {
	if version == nil {
		return invalid(path, "missing")
	}
	if version.GetMajor() == 0 {
		return invalid(path+".major", "must be positive")
	}
	if version.GetMinMinor() > version.GetMaxMinor() {
		return invalid(path, "minor range is inverted")
	}
	return nil
}

// ValidateCapability validates one capability declaration or grant version.
func ValidateCapability(path string, capability *CapabilityVersion) error {
	if capability == nil {
		return invalid(path, "missing")
	}
	if err := validateIdentifier(path+".name", capability.GetName()); err != nil {
		return err
	}
	if capability.GetMajor() == 0 {
		return invalid(path+".major", "must be positive")
	}
	if capability.GetMinMinor() > capability.GetMaxMinor() {
		return invalid(path, "minor range is inverted")
	}
	return nil
}

// ValidateResourceLimits validates advertised or negotiated budgets. Zero
// means that the value is unspecified and lets the application choose a
// default; non-zero values must be internally consistent.
func ValidateResourceLimits(limits *ResourceLimits) error {
	if limits == nil {
		return nil
	}
	if limits.GetMaxEventBytes() != 0 && limits.GetMaxOutputBytes() != 0 &&
		limits.GetMaxEventBytes() > limits.GetMaxOutputBytes() {
		return invalid("limits.max_event_bytes", "exceeds max_output_bytes")
	}
	return nil
}

// ValidateReadyResponse validates the semantic readiness status returned by a
// plugin process. Unknown proto enum numbers are rejected rather than being
// silently treated as an unspecified status.
func ValidateReadyResponse(response *ReadyResponse) error {
	if response == nil {
		return invalid("ready", "missing")
	}
	switch response.GetStatus() {
	case ReadinessStatus_READINESS_STATUS_READY,
		ReadinessStatus_READINESS_STATUS_NOT_READY,
		ReadinessStatus_READINESS_STATUS_DEGRADED:
		// Known statuses are valid.
	case ReadinessStatus_READINESS_STATUS_UNSPECIFIED:
		return invalid("ready.status", "must be specified")
	default:
		return invalid("ready.status", "unsupported")
	}
	return validateText("ready.message", response.GetMessage(), maxEventMessageBytes, true)
}

// ValidateHealthResponse validates the semantic health status returned by a
// plugin process.
func ValidateHealthResponse(response *HealthResponse) error {
	if response == nil {
		return invalid("health", "missing")
	}
	switch response.GetStatus() {
	case HealthStatus_HEALTH_STATUS_SERVING,
		HealthStatus_HEALTH_STATUS_NOT_SERVING,
		HealthStatus_HEALTH_STATUS_DEGRADED:
		// Known statuses are valid.
	case HealthStatus_HEALTH_STATUS_UNSPECIFIED:
		return invalid("health.status", "must be specified")
	default:
		return invalid("health.status", "unsupported")
	}
	return validateText("health.message", response.GetMessage(), maxEventMessageBytes, true)
}

// ValidateShutdownRequest validates a graceful or immediate shutdown request.
func ValidateShutdownRequest(request *ShutdownRequest) error {
	if request == nil {
		return invalid("shutdown", "missing")
	}
	switch request.GetMode() {
	case ShutdownMode_SHUTDOWN_MODE_GRACEFUL,
		ShutdownMode_SHUTDOWN_MODE_IMMEDIATE:
		return nil
	case ShutdownMode_SHUTDOWN_MODE_UNSPECIFIED:
		return invalid("shutdown.mode", "must be specified")
	default:
		return invalid("shutdown.mode", "unsupported")
	}
}

// ValidateConfigureRequest validates the application-selected protocol and grants.
func ValidateConfigureRequest(request *ConfigureRequest) error {
	if request == nil {
		return invalid("configure", "missing")
	}
	if err := validateProtocolVersion("application_protocol", request.GetApplicationProtocol()); err != nil {
		return err
	}
	if len(request.GetGrants()) > maxCapabilityCount {
		return invalid("grants", "too many")
	}
	seen := make(map[string]struct{}, len(request.GetGrants()))
	for i, grant := range request.GetGrants() {
		path := fmt.Sprintf("grants[%d]", i)
		if grant == nil {
			return invalid(path, "missing")
		}
		if err := ValidateCapability(path+".capability", grant.GetCapability()); err != nil {
			return err
		}
		name := grant.GetCapability().GetName()
		if _, ok := seen[name]; ok {
			return invalid(path+".capability.name", "duplicate")
		}
		seen[name] = struct{}{}
	}
	if err := ValidateResourceLimits(request.GetLimits()); err != nil {
		return err
	}
	if len(request.GetConfigurationJson()) != 0 {
		if err := validateJSONObject("configuration_json", request.GetConfigurationJson(), maxJSONSchemaBytes); err != nil {
			return err
		}
	}
	return nil
}

func validateProtocolVersion(path string, version *ProtocolVersion) error {
	if version == nil {
		return invalid(path, "missing")
	}
	if version.GetMajor() == 0 {
		return invalid(path+".major", "must be positive")
	}
	return nil
}

// ValidateToolDescriptor validates one immutable tools.v1 declaration.
func ValidateToolDescriptor(path string, tool *ToolDescriptor) error {
	if tool == nil {
		return invalid(path, "missing")
	}
	if err := validateIdentifier(path+".id", tool.GetId()); err != nil {
		return err
	}
	if err := validateIdentifier(path+".name", tool.GetName()); err != nil {
		return err
	}
	if err := validateText(path+".description", tool.GetDescription(), maxDescriptionBytes, true); err != nil {
		return err
	}
	if err := ValidateToolHints(path+".hints", tool.GetHints()); err != nil {
		return err
	}
	if err := validateJSONObject(path+".input_schema_json", tool.GetInputSchemaJson(), maxJSONSchemaBytes); err != nil {
		return err
	}
	return nil
}

// ValidateToolHints validates optional, untrusted tool annotations.
func ValidateToolHints(path string, hints *ToolHints) error {
	if hints == nil {
		return nil
	}
	switch hints.GetRisk() {
	case RiskHint_RISK_HINT_UNSPECIFIED,
		RiskHint_RISK_HINT_READ_ONLY,
		RiskHint_RISK_HINT_STATE_CHANGE,
		RiskHint_RISK_HINT_PRIVILEGED:
		return nil
	default:
		return invalid(path+".risk", "unsupported")
	}
}

// ValidateToolList validates a complete immutable tool declaration snapshot.
func ValidateToolList(response *ListToolsResponse) error {
	if response == nil {
		return invalid("tools", "missing")
	}
	if len(response.GetTools()) > maxToolCount {
		return invalid("tools", "too many")
	}
	seenIDs := make(map[string]struct{}, len(response.GetTools()))
	seenNames := make(map[string]struct{}, len(response.GetTools()))
	for i, tool := range response.GetTools() {
		path := fmt.Sprintf("tools[%d]", i)
		if err := ValidateToolDescriptor(path, tool); err != nil {
			return err
		}
		if _, ok := seenIDs[tool.GetId()]; ok {
			return invalid(path+".id", "duplicate")
		}
		if _, ok := seenNames[tool.GetName()]; ok {
			return invalid(path+".name", "duplicate")
		}
		seenIDs[tool.GetId()] = struct{}{}
		seenNames[tool.GetName()] = struct{}{}
	}
	return nil
}

// ValidateInvokeToolRequest validates an invocation envelope. The gRPC
// context remains authoritative for cancellation and deadlines.
func ValidateInvokeToolRequest(request *InvokeToolRequest) error {
	if request == nil {
		return invalid("invoke", "missing")
	}
	if err := validateIdentifier("tool_id", request.GetToolId()); err != nil {
		return err
	}
	if err := validateIdentifier("invocation_id", request.GetInvocationId()); err != nil {
		return err
	}
	if request.GetDeadlineUnixNanos() < 0 {
		return invalid("deadline_unix_nanos", "must not be negative")
	}
	return validateJSONObject("arguments_json", request.GetArgumentsJson(), maxJSONSchemaBytes)
}

// ValidateToolEvent validates one stream item. Use ToolStreamValidator to
// additionally enforce ordering and the exactly-one-terminal-event rule.
func ValidateToolEvent(event *ToolEvent) error {
	if event == nil {
		return invalid("event", "missing")
	}
	if event.GetSequence() == 0 {
		return invalid("sequence", "must be positive")
	}
	switch payload := event.GetPayload().(type) {
	case *InvokeToolResponse_Progress:
		return validateProgress(payload.Progress)
	case *InvokeToolResponse_Result:
		return validateResult(payload.Result)
	case *InvokeToolResponse_Failure:
		return validateFailure(payload.Failure)
	default:
		return invalid("payload", "missing")
	}
}

func validateProgress(progress *ToolProgress) error {
	if progress == nil {
		return invalid("progress", "missing")
	}
	if err := validateText("progress.message", progress.GetMessage(), maxEventMessageBytes, true); err != nil {
		return err
	}
	if progress.GetPercentMillis() > 100000 {
		return invalid("progress.percent_millis", "must be at most 100000")
	}
	if len(progress.GetMetadataJson()) != 0 {
		if len(progress.GetMetadataJson()) > maxProgressMetadata {
			return invalid("progress.metadata_json", "too large")
		}
		return validateJSONObject("progress.metadata_json", progress.GetMetadataJson(), maxProgressMetadata)
	}
	return nil
}

func validateResult(result *ToolResult) error {
	if result == nil {
		return invalid("result", "missing")
	}
	var total int
	for i, content := range result.GetContent() {
		path := fmt.Sprintf("result.content[%d]", i)
		if content == nil || content.GetMediaType() == "" {
			return invalid(path, "media_type is required")
		}
		if err := validateText(path+".media_type", content.GetMediaType(), maxIdentifierBytes, false); err != nil {
			return err
		}
		if len(content.GetData()) > maxToolContentBytes {
			return invalid(path+".data", "too large")
		}
		total += len(content.GetData())
	}
	if total+len(result.GetStructuredJson()) > maxToolResultBytes {
		return invalid("result", "too large")
	}
	if len(result.GetStructuredJson()) != 0 {
		if err := validateJSONObject("result.structured_json", result.GetStructuredJson(), maxToolResultBytes); err != nil {
			return err
		}
	}
	return nil
}

func validateFailure(failure *ToolFailure) error {
	if failure == nil {
		return invalid("failure", "missing")
	}
	switch failure.GetCode() {
	case FailureCode_FAILURE_CODE_INVALID_ARGUMENT,
		FailureCode_FAILURE_CODE_NOT_FOUND,
		FailureCode_FAILURE_CODE_PERMISSION_DENIED,
		FailureCode_FAILURE_CODE_CANCELED,
		FailureCode_FAILURE_CODE_DEADLINE_EXCEEDED,
		FailureCode_FAILURE_CODE_OUTPUT_LIMIT,
		FailureCode_FAILURE_CODE_UNAVAILABLE,
		FailureCode_FAILURE_CODE_INTERNAL:
		// Known failure codes are valid.
	case FailureCode_FAILURE_CODE_UNSPECIFIED:
		return invalid("failure.code", "must be specified")
	default:
		return invalid("failure.code", "unsupported")
	}
	if err := validateText("failure.message", failure.GetMessage(), maxEventMessageBytes, true); err != nil {
		return err
	}
	if len(failure.GetProblems()) > maxFailureProblems {
		return invalid("failure.problems", "too many")
	}
	for i, problem := range failure.GetProblems() {
		path := fmt.Sprintf("failure.problems[%d]", i)
		if problem == nil {
			return invalid(path, "missing")
		}
		if err := validateText(path+".field", problem.GetField(), maxIdentifierBytes, false); err != nil {
			return err
		}
		if err := validateText(path+".reason", problem.GetReason(), maxEventMessageBytes, false); err != nil {
			return err
		}
		if err := validateText(path+".hint", problem.GetHint(), maxEventMessageBytes, true); err != nil {
			return err
		}
	}
	if len(failure.GetDetailsJson()) > maxFailureDetailsBytes {
		return invalid("failure.details_json", "too large")
	}
	if len(failure.GetDetailsJson()) != 0 {
		return validateJSONObject("failure.details_json", failure.GetDetailsJson(), maxFailureDetailsBytes)
	}
	return nil
}

// ToolStreamValidator validates one InvokeTool stream in order. A zero value
// is ready for use and can be discarded after Done returns nil.
type ToolStreamValidator struct {
	lastSequence uint64
	terminal     bool
}

// Validate accepts progress until a terminal result/failure is observed and
// rejects gaps in ownership such as events after the terminal item.
func (v *ToolStreamValidator) Validate(event *ToolEvent) error {
	if v == nil {
		return invalid("stream_validator", "nil")
	}
	if err := ValidateToolEvent(event); err != nil {
		return err
	}
	if v.terminal {
		return invalid("event", "after terminal event")
	}
	if event.GetSequence() <= v.lastSequence {
		return invalid("sequence", "not increasing")
	}
	v.lastSequence = event.GetSequence()
	switch event.GetPayload().(type) {
	case *InvokeToolResponse_Result, *InvokeToolResponse_Failure:
		v.terminal = true
	}
	return nil
}

// Done requires exactly one terminal event.
func (v *ToolStreamValidator) Done() error {
	if v == nil || !v.terminal {
		return invalid("stream", "missing terminal event")
	}
	return nil
}

func validateIdentifier(path, value string) error {
	if value == "" {
		return invalid(path, "required")
	}
	if len(value) > maxIdentifierBytes {
		return invalid(path, "too large")
	}
	if !utf8.ValidString(value) {
		return invalid(path, "invalid utf-8")
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		if i == 0 {
			return invalid(path, "must start with a lowercase letter or digit")
		}
		return invalid(path, "contains unsupported character")
	}
	return nil
}

func validateText(path, value string, maxBytes int, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return invalid(path, "required")
	}
	if len(value) > maxBytes {
		return invalid(path, "too large")
	}
	if !utf8.ValidString(value) {
		return invalid(path, "invalid utf-8")
	}
	for _, r := range value {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\r' && r != '\t') {
			return invalid(path, "contains control character")
		}
	}
	return nil
}

func validateJSONObject(path string, value []byte, maxBytes int) error {
	if len(value) == 0 {
		return invalid(path, "required")
	}
	if len(value) > maxBytes {
		return invalid(path, "too large")
	}
	if !utf8.Valid(value) {
		return invalid(path, "must be valid utf-8 json")
	}

	var decoded any
	if err := jsonx.Decode(value, &decoded); err != nil {
		return invalid(path, "must be one strict json object")
	}
	if _, ok := decoded.(map[string]any); !ok {
		return invalid(path, "root must be an object")
	}
	return nil
}
