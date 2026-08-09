// Package hooks owns the Pips-native Coding lifecycle hook configuration,
// trust records, and reviewed command protocol.
package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// Schema identifies the strict lifecycle hook configuration format.
	Schema = "pips.coding.hooks/v1alpha1"
	// InputSchema identifies the JSON object written to one hook command stdin.
	InputSchema = "pips.coding.hook-input/v1alpha1"
	// TrustSchema identifies the Pips-owned hook trust store format.
	TrustSchema = "pips.coding.hook-trust/v1alpha1"

	defaultTimeout      = 10 * time.Minute
	defaultEndTimeout   = time.Second
	maximumTimeout      = 10 * time.Minute
	maximumEndTimeout   = 3 * time.Second
	maximumFileBytes    = 1 << 20
	maximumHandlers     = 128
	maximumCommand      = 64 << 10
	maximumMatcher      = 4 << 10
	maximumInputBytes   = 1 << 20
	maximumOutputBytes  = 64 << 10
	maximumErrorBytes   = 8 << 10
	maximumContext      = 16 << 10
	maximumReason       = 4 << 10
	maximumTrustRecords = 4096
)

var (
	// ErrInvalid reports malformed hook configuration, protocol data, or options.
	ErrInvalid = errors.New("coding hooks: invalid")
	// ErrLimitExceeded reports one configured or runtime hook bound being exceeded.
	ErrLimitExceeded = errors.New("coding hooks: limit exceeded")
	// ErrUnsafeFile reports a hook configuration or trust file that cannot be trusted.
	ErrUnsafeFile = errors.New("coding hooks: unsafe file")
	// ErrUntrusted reports an attempted project action before workspace trust.
	ErrUntrusted = errors.New("coding hooks: workspace is not trusted")
)

// Event identifies one supported Coding lifecycle boundary.
type Event string

// Supported lifecycle events.
const (
	EventSessionStart      Event = "SessionStart"
	EventUserPromptSubmit  Event = "UserPromptSubmit"
	EventPreToolUse        Event = "PreToolUse"
	EventPermissionRequest Event = "PermissionRequest"
	EventPostToolUse       Event = "PostToolUse"
	EventPreCompact        Event = "PreCompact"
	EventPostCompact       Event = "PostCompact"
	EventSubagentStart     Event = "SubagentStart"
	EventSubagentStop      Event = "SubagentStop"
	EventStop              Event = "Stop"
	EventSessionEnd        Event = "SessionEnd"
)

var eventOrder = []Event{
	EventSessionStart,
	EventUserPromptSubmit,
	EventPreToolUse,
	EventPermissionRequest,
	EventPostToolUse,
	EventPreCompact,
	EventPostCompact,
	EventSubagentStart,
	EventSubagentStop,
	EventStop,
	EventSessionEnd,
}

// Scope identifies the file that contributed a handler.
type Scope string

// Supported configuration scopes.
const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// Visibility controls whether a trusted definition is mandatory ambient
// policy or can only be added to one exact custom child plan.
type Visibility string

const (
	// VisibilityAmbient makes a Hook mandatory ambient policy.
	VisibilityAmbient Visibility = "ambient"
	// VisibilityAgentPrivate makes a Hook selectable by exact custom child ID.
	VisibilityAgentPrivate Visibility = "agent_private"
)

// Status is the local trust state of one loaded handler.
type Status string

// Supported handler trust states.
const (
	StatusPending Status = "pending"
	StatusTrusted Status = "trusted"
)

// Limits bound untrusted hook configuration and trusted command protocol data.
type Limits struct {
	MaxFileBytes   int64
	MaxHandlers    int
	MaxCommand     int
	MaxMatcher     int
	MaxInputBytes  int
	MaxOutputBytes int
	MaxErrorBytes  int
	MaxContext     int
	MaxReason      int
}

// DefaultLimits returns the fixed V1 lifecycle hook bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes: maximumFileBytes, MaxHandlers: maximumHandlers,
		MaxCommand: maximumCommand, MaxMatcher: maximumMatcher,
		MaxInputBytes: maximumInputBytes, MaxOutputBytes: maximumOutputBytes,
		MaxErrorBytes: maximumErrorBytes, MaxContext: maximumContext,
		MaxReason: maximumReason,
	}
}

// Definition is one immutable, validated command handler.
type Definition struct {
	ID         string
	Reference  string
	Scope      Scope
	Source     string
	Visibility Visibility
	Event      Event
	Matcher    string
	Command    string
	Timeout    time.Duration

	matcher *regexp.Regexp
}

// Fingerprint returns the normalized semantic SHA-256 identity of Definition.
func (d Definition) Fingerprint() string {
	encoded, _ := json.Marshal(struct {
		ID         string     `json:"id,omitempty"`
		Visibility Visibility `json:"visibility,omitempty"`
		Event      Event      `json:"event"`
		Matcher    string     `json:"matcher,omitempty"`
		Type       string     `json:"type"`
		Command    string     `json:"command"`
		TimeoutNS  int64      `json:"timeout_ns"`
	}{
		ID: d.ID, Visibility: d.privateVisibility(), Event: d.Event,
		Matcher: d.Matcher, Type: "command", Command: d.Command,
		TimeoutNS: d.Timeout.Nanoseconds(),
	})
	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:])
}

func (d Definition) privateVisibility() Visibility {
	if d.effectiveVisibility() == VisibilityAgentPrivate {
		return VisibilityAgentPrivate
	}

	return ""
}

// EffectiveVisibility normalizes the zero value to the legacy ambient policy.
func (d Definition) EffectiveVisibility() Visibility { return d.effectiveVisibility() }

func (d Definition) effectiveVisibility() Visibility {
	if d.Visibility == "" {
		return VisibilityAmbient
	}

	return d.Visibility
}

// Matches reports whether Definition applies to one lifecycle invocation.
func (d Definition) Matches(event Event, target string) bool {
	if d.Event != event {
		return false
	}
	if !event.usesMatcher() {
		return true
	}
	if d.matcher == nil {
		return true
	}
	for _, candidate := range event.matcherTargets(target) {
		if d.matcher.MatchString(candidate) {
			return true
		}
	}

	return false
}

// Definitions is an immutable validated handler snapshot.
type Definitions struct {
	values []Definition
}

// List returns a defensive copy in source/event/group/handler order.
func (d Definitions) List() []Definition { return slices.Clone(d.values) }

// Matching returns definitions that match one event and event target.
func (d Definitions) Matching(event Event, target string) []Definition {
	values := make([]Definition, 0, len(d.values))
	for _, definition := range d.values {
		if definition.Matches(event, target) {
			values = append(values, definition)
		}
	}

	return values
}

// ResolvedDefinition attaches the effective local trust state.
type ResolvedDefinition struct {
	Definition Definition
	Status     Status
}

// Diagnostic describes a non-fatal lifecycle hook suppression or command failure.
type Diagnostic struct {
	Reference string
	Code      string
	Message   string
}

// Invocation supplies one JSON payload to matching trusted command handlers.
type Invocation struct {
	Event  Event
	Target string
	Input  any
}

// Outcome is the merged result of one hook invocation.
type Outcome struct {
	// Blocked is the event-specific Codex-style block decision. Runtime glue
	// maps it to prompt/tool denial, tool feedback, or a continuation request.
	Blocked bool
	// Allowed is meaningful to PermissionRequest and PreToolUse. It never
	// bypasses subsequent Pips policy, approval, or sandbox checks.
	Allowed bool
	// Stopped records a continue:false response. It takes precedence over a
	// continuation request for events that support continuation control.
	Stopped bool
	Reason  string
	Context []string
	// UpdatedInput is a validated replacement JSON arguments object for an
	// allowed PreToolUse invocation.
	UpdatedInput json.RawMessage
	Diagnostics  []Diagnostic
}

func (e Event) valid() bool {
	for _, candidate := range eventOrder {
		if e == candidate {
			return true
		}
	}

	return false
}

func (e Event) usesMatcher() bool {
	switch e {
	case EventSessionStart, EventSessionEnd, EventPreToolUse,
		EventPermissionRequest, EventPostToolUse, EventPreCompact,
		EventPostCompact, EventSubagentStart, EventSubagentStop:
		return true
	default:
		return false
	}
}

func (e Event) matcherTargets(target string) []string {
	if e != EventPreToolUse && e != EventPermissionRequest && e != EventPostToolUse {
		return []string{target}
	}

	values := []string{target}
	switch target {
	case "shell":
		values = append(values, "Bash")
	case "apply_patch":
		values = append(values, "Edit", "Write")
	case "run_subagent", "spawn_agent":
		values = append(values, "Agent")
	}

	return values
}

func (e Event) acceptsAdditionalContext() bool {
	switch e {
	case EventSessionStart, EventUserPromptSubmit, EventSubagentStart,
		EventPreToolUse, EventPostToolUse:
		return true
	default:
		return false
	}
}

func (e Event) acceptsPlainContext() bool {
	switch e {
	case EventSessionStart, EventUserPromptSubmit, EventSubagentStart:
		return true
	default:
		return false
	}
}

func (e Event) requiresJSONOutput() bool {
	return e == EventStop || e == EventSubagentStop
}

func (e Event) acceptsBlock() bool {
	switch e {
	case EventUserPromptSubmit, EventPreToolUse, EventPermissionRequest,
		EventPostToolUse, EventSubagentStop, EventStop:
		return true
	default:
		return false
	}
}

func (e Event) acceptsContinueControl() bool {
	switch e {
	case EventSessionStart, EventUserPromptSubmit, EventPostToolUse,
		EventPreCompact, EventPostCompact, EventSubagentStart, EventSubagentStop,
		EventStop:
		return true
	default:
		return false
	}
}

func (s Scope) valid() bool { return s == ScopeUser || s == ScopeProject }

func (l Limits) valid() bool {
	return l.MaxFileBytes > 0 && l.MaxHandlers > 0 && l.MaxCommand > 0 &&
		l.MaxMatcher > 0 && l.MaxInputBytes > 0 && l.MaxOutputBytes > 0 &&
		l.MaxErrorBytes > 0 && l.MaxContext > 0 && l.MaxReason > 0
}

func validateDefinition(definition Definition, limits Limits) error {
	if !definition.Scope.valid() || !definition.Event.valid() || definition.Reference == "" ||
		definition.Source == "" || strings.TrimSpace(definition.Command) == "" {
		return errors.New("incomplete definition")
	}
	if len(definition.Command) > limits.MaxCommand || len(definition.Matcher) > limits.MaxMatcher {
		return ErrLimitExceeded
	}
	if definition.Timeout <= 0 || definition.Timeout > maximumTimeout ||
		definition.Event == EventSessionEnd && definition.Timeout > maximumEndTimeout {
		return errors.New("invalid timeout")
	}

	visibility := definition.effectiveVisibility()
	if visibility != VisibilityAmbient && visibility != VisibilityAgentPrivate {
		return errors.New("invalid visibility")
	}

	if visibility == VisibilityAmbient && definition.ID != "" {
		return errors.New("ambient hook cannot set an ID")
	}

	if visibility == VisibilityAgentPrivate {
		if !validPrivateID(definition.ID) {
			return errors.New("agent-private hook requires a canonical ID")
		}

		switch definition.Event {
		case EventPreToolUse, EventPermissionRequest, EventPostToolUse:
		default:
			return errors.New("agent-private hook event is not child-scoped")
		}
	}
	return nil
}

func validPrivateID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}

	separator := true

	for _, current := range value {
		if current >= 'a' && current <= 'z' || current >= '0' && current <= '9' {
			separator = false
			continue
		}

		if (current == '-' || current == '_') && !separator {
			separator = true
			continue
		}

		return false
	}

	return !separator
}

func compileDefinition(definition Definition, limits Limits) (Definition, error) {
	if definition.Matcher == "*" {
		definition.Matcher = ""
	}
	if err := validateDefinition(definition, limits); err != nil {
		return Definition{}, err
	}
	if definition.Matcher != "" {
		compiled, err := regexp.Compile(definition.Matcher)
		if err != nil {
			return Definition{}, fmt.Errorf("invalid matcher: %w", err)
		}
		definition.matcher = compiled
	}

	return definition, nil
}
