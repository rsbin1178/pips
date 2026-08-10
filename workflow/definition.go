package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	// SchemaV1Alpha1 identifies the first Workflow Definition wire format.
	SchemaV1Alpha1 = "pips.workflow/v1alpha1"

	maxDefinitionBytes = 2 << 20
	maxDefinitionNodes = 256
	maxDefinitionEdges = 1_024
	maxConfigBytes     = 256 << 10
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// DefinitionID identifies a logical workflow across revisions.
type DefinitionID string

// Revision identifies one immutable Definition revision.
type Revision string

// NodeID identifies a node inside one Definition.
type NodeID string

// NodeTypeKey identifies a registered node implementation family.
type NodeTypeKey string

// ActionKey identifies a registered host Action family.
type ActionKey string

// BindingSource identifies where an input value comes from.
type BindingSource string

// Binding sources.
const (
	BindingLiteral       BindingSource = "literal"
	BindingWorkflowInput BindingSource = "workflow_input"
	BindingNodeOutput    BindingSource = "node_output"
	BindingLoopVariable  BindingSource = "loop_variable"
)

// Binding selects a literal, Workflow input, prior node output, or direct Loop
// variable. Path walks object keys or decimal array indexes after resolving the
// source value.
type Binding struct {
	Source BindingSource `json:"source"`
	Node   NodeID        `json:"node,omitempty"`
	Port   string        `json:"port,omitempty"`
	Path   []string      `json:"path,omitempty"`
	Value  *Value        `json:"value,omitempty"`
}

// OutputBinding declares one Workflow output contract and its value source.
type OutputBinding struct {
	Schema  PortSchema `json:"schema"`
	Binding Binding    `json:"binding"`
}

// NodeRoute selects one named control route from a node.
type NodeRoute struct {
	Node  NodeID `json:"node"`
	Route string `json:"route"`
}

// ControlEdge connects one named source route to a target node. It carries no
// data; values move only through explicit Bindings.
type ControlEdge struct {
	From NodeRoute `json:"from"`
	To   NodeID    `json:"to"`
}

// RetryPolicy bounds retries for one node. Zero MaxAttempts means one attempt
// and therefore no retry.
type RetryPolicy struct {
	MaxAttempts  int   `json:"max_attempts,omitempty"`
	BackoffMilli int64 `json:"backoff_ms,omitempty"`
}

// ErrorAction controls a node failure after retries are exhausted.
type ErrorAction string

// Node failure actions. The zero value is equivalent to ErrorStop.
const (
	ErrorStop                ErrorAction = "stop"
	ErrorRoute               ErrorAction = "route_error"
	ErrorContinueWithDefault ErrorAction = "continue_with_default"
)

// NodePolicy bounds one node invocation and defines its explicit error path.
type NodePolicy struct {
	TimeoutMilli   int64            `json:"timeout_ms,omitempty"`
	Retry          RetryPolicy      `json:"retry,omitzero"`
	Error          ErrorAction      `json:"error,omitempty"`
	DefaultOutputs map[string]Value `json:"default_outputs,omitempty"`
}

// Limits bounds one Run. Both fields must be positive.
type Limits struct {
	MaxConcurrency int `json:"max_concurrency"`
	MaxSteps       int `json:"max_steps"`
}

// DefaultLimits returns conservative in-process Run limits.
func DefaultLimits() Limits {
	return Limits{MaxConcurrency: 4, MaxSteps: 1_000}
}

// NodeDefinition is one version-pinned node instance.
type NodeDefinition struct {
	ID      NodeID             `json:"id"`
	Name    string             `json:"name,omitempty"`
	Type    NodeTypeKey        `json:"type"`
	Version string             `json:"version"`
	Config  json.RawMessage    `json:"config,omitempty"`
	Inputs  map[string]Binding `json:"inputs,omitempty"`
	Policy  NodePolicy         `json:"policy,omitzero"`
}

// Definition is a versioned, serializable Workflow contract.
type Definition struct {
	Schema   string       `json:"schema"`
	ID       DefinitionID `json:"id"`
	Revision Revision     `json:"revision"`
	Name     string       `json:"name"`

	Inputs  map[string]PortSchema    `json:"inputs"`
	Outputs map[string]OutputBinding `json:"outputs"`
	Nodes   []NodeDefinition         `json:"nodes"`
	Edges   []ControlEdge            `json:"edges"`
	Limits  Limits                   `json:"limits"`
}

// DecodeDefinition strictly decodes and validates one Definition snapshot.
func DecodeDefinition(data []byte) (Definition, error) {
	limits := defaultJSONLimits
	limits.maxBytes = maxDefinitionBytes

	var definition Definition
	if err := decodeJSON(data, &definition, limits, true); err != nil {
		return Definition{}, fmt.Errorf("%w: decode: %w", ErrInvalidDefinition, err)
	}

	if err := validateDefinitionShape(definition); err != nil {
		return Definition{}, err
	}

	return definition, nil
}

// Fingerprint returns a SHA-256 digest of execution-semantic fields. ID,
// Revision, and Name do not affect it.
func (d Definition) Fingerprint() (string, error) {
	snapshot, err := cloneDefinition(d)
	if err != nil {
		return "", err
	}

	snapshot.ID = ""
	snapshot.Revision = ""
	snapshot.Name = ""

	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("%w: encode fingerprint: %w", ErrInvalidDefinition, err)
	}

	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:]), nil
}

func cloneDefinition(definition Definition) (Definition, error) {
	data, err := json.Marshal(definition)
	if err != nil {
		return Definition{}, fmt.Errorf("%w: encode snapshot: %w", ErrInvalidDefinition, err)
	}

	return DecodeDefinition(data)
}

func validateDefinitionShape(definition Definition) error {
	if err := validateDefinitionIdentity(definition); err != nil {
		return err
	}

	if err := validateDefinitionSize(definition); err != nil {
		return err
	}

	if err := validateLimits(definition.Limits); err != nil {
		return err
	}

	if err := validateDefinitionPorts(definition); err != nil {
		return err
	}

	return validateDefinitionNodes(definition)
}

func validateDefinitionIdentity(definition Definition) error {
	if definition.Schema != SchemaV1Alpha1 {
		return invalidDefinitionf("unsupported schema %q", definition.Schema)
	}

	if !validIdentifier(string(definition.ID)) {
		return invalidDefinitionf("invalid definition id %q", definition.ID)
	}

	if !validIdentifier(string(definition.Revision)) {
		return invalidDefinitionf("invalid revision %q", definition.Revision)
	}

	if strings.TrimSpace(definition.Name) == "" || len(definition.Name) > 256 {
		return invalidDefinitionf("invalid name")
	}

	return nil
}

func validateDefinitionSize(definition Definition) error {
	if definition.Inputs == nil || definition.Outputs == nil || definition.Nodes == nil || definition.Edges == nil {
		return invalidDefinitionf("inputs, outputs, nodes, and edges must be present")
	}

	if len(definition.Nodes) == 0 || len(definition.Nodes) > maxDefinitionNodes {
		return invalidDefinitionf("node count must be between 1 and %d", maxDefinitionNodes)
	}

	if len(definition.Edges) > maxDefinitionEdges {
		return invalidDefinitionf("edge count exceeds %d", maxDefinitionEdges)
	}

	return nil
}

func validateDefinitionPorts(definition Definition) error {
	for name, schema := range definition.Inputs {
		if !validIdentifier(name) || !schema.IsValid() {
			return invalidDefinitionf("invalid input %q", name)
		}
	}

	for name, output := range definition.Outputs {
		if !validIdentifier(name) || !output.Schema.IsValid() {
			return invalidDefinitionf("invalid output %q", name)
		}

		if err := validateBinding(output.Binding); err != nil {
			return invalidDefinitionf("output %q: %v", name, err)
		}
	}

	return nil
}

func validateDefinitionNodes(definition Definition) error {
	seen := make(map[NodeID]struct{}, len(definition.Nodes))
	for _, node := range definition.Nodes {
		if _, duplicate := seen[node.ID]; duplicate {
			return invalidDefinitionf("duplicate node id %q", node.ID)
		}

		seen[node.ID] = struct{}{}
		if err := validateNodeDefinition(node); err != nil {
			return err
		}
	}

	return validateDefinitionEdges(definition.Edges)
}

func validateNodeDefinition(node NodeDefinition) error {
	if !validIdentifier(string(node.ID)) || !validIdentifier(string(node.Type)) ||
		!validIdentifier(node.Version) {
		return invalidDefinitionf("invalid node identity %q", node.ID)
	}

	if len(node.Name) > 256 || len(node.Config) > maxConfigBytes {
		return invalidDefinitionf("node %q name or config exceeds limit", node.ID)
	}

	for name, binding := range node.Inputs {
		if !validIdentifier(name) {
			return invalidDefinitionf("node %q has invalid input %q", node.ID, name)
		}

		if err := validateBinding(binding); err != nil {
			return invalidDefinitionf("node %q input %q: %v", node.ID, name, err)
		}
	}

	if err := validateNodePolicy(node.Policy); err != nil {
		return invalidDefinitionf("node %q: %v", node.ID, err)
	}

	return nil
}

func validateDefinitionEdges(edges []ControlEdge) error {
	for _, edge := range edges {
		if !validIdentifier(string(edge.From.Node)) || !validIdentifier(edge.From.Route) ||
			!validIdentifier(string(edge.To)) {
			return invalidDefinitionf("invalid control edge")
		}
	}

	return nil
}

func validateBinding(binding Binding) error {
	if err := validateBindingPath(binding.Path); err != nil {
		return err
	}

	return validateBindingSource(binding)
}

func validateBindingPath(path []string) error {
	for _, segment := range path {
		if segment == "" || len(segment) > 128 {
			return fmt.Errorf("invalid path segment %q", segment)
		}
	}

	return nil
}

func validateBindingSource(binding Binding) error {
	switch binding.Source {
	case BindingLiteral:
		return validateLiteralBinding(binding)
	case BindingWorkflowInput:
		return validateWorkflowInputBinding(binding)
	case BindingNodeOutput:
		return validateNodeOutputBinding(binding)
	case BindingLoopVariable:
		return validateLoopVariableBinding(binding)
	default:
		return fmt.Errorf("unknown binding source %q", binding.Source)
	}
}

func validateLoopVariableBinding(binding Binding) error {
	if binding.Value != nil || binding.Node != "" || !validIdentifier(binding.Port) {
		return errors.New("loop variable binding requires only port")
	}

	return nil
}

func validateLiteralBinding(binding Binding) error {
	if binding.Value == nil || !binding.Value.IsValid() || binding.Node != "" || binding.Port != "" {
		return errors.New("literal binding requires only value")
	}

	return nil
}

func validateWorkflowInputBinding(binding Binding) error {
	if binding.Value != nil || binding.Node != "" || !validIdentifier(binding.Port) {
		return errors.New("workflow input binding requires only port")
	}

	return nil
}

func validateNodeOutputBinding(binding Binding) error {
	if binding.Value != nil || !validIdentifier(string(binding.Node)) || !validIdentifier(binding.Port) {
		return errors.New("node output binding requires node and port")
	}

	return nil
}

func validateLimits(limits Limits) error {
	if limits.MaxConcurrency <= 0 || limits.MaxConcurrency > maxDefinitionNodes {
		return invalidDefinitionf("max concurrency must be between 1 and %d", maxDefinitionNodes)
	}

	if limits.MaxSteps <= 0 || limits.MaxSteps > 1_000_000 {
		return invalidDefinitionf("max steps must be between 1 and 1000000")
	}

	return nil
}

func validateNodePolicy(policy NodePolicy) error {
	if err := validateNodePolicyBounds(policy); err != nil {
		return err
	}

	if err := validateNodeErrorAction(policy); err != nil {
		return err
	}

	for name, value := range policy.DefaultOutputs {
		if !validIdentifier(name) || !value.IsValid() {
			return fmt.Errorf("invalid default output %q", name)
		}
	}

	return nil
}

func validateNodePolicyBounds(policy NodePolicy) error {
	if policy.TimeoutMilli < 0 || policy.TimeoutMilli > 86_400_000 {
		return errors.New("timeout_ms must be between 0 and 86400000")
	}

	if policy.Retry.MaxAttempts < 0 || policy.Retry.MaxAttempts > 10 {
		return errors.New("max_attempts must be between 0 and 10")
	}

	if policy.Retry.BackoffMilli < 0 || policy.Retry.BackoffMilli > 3_600_000 {
		return errors.New("backoff_ms must be between 0 and 3600000")
	}

	return nil
}

func validateNodeErrorAction(policy NodePolicy) error {
	switch policy.Error {
	case "", ErrorStop, ErrorRoute:
		if len(policy.DefaultOutputs) != 0 {
			return errors.New("default outputs require continue_with_default")
		}
	case ErrorContinueWithDefault:
		if len(policy.DefaultOutputs) == 0 {
			return errors.New("continue_with_default requires default outputs")
		}
	default:
		return fmt.Errorf("unknown error action %q", policy.Error)
	}

	return nil
}

func cloneBinding(binding Binding) Binding {
	binding.Path = slices.Clone(binding.Path)
	if binding.Value != nil {
		value := *binding.Value
		binding.Value = &value
	}

	return binding
}

func cloneNodeDefinition(definition NodeDefinition) NodeDefinition {
	definition.Config = slices.Clone(definition.Config)
	if definition.Inputs != nil {
		inputs := definition.Inputs

		definition.Inputs = make(map[string]Binding, len(inputs))
		for name, binding := range inputs {
			definition.Inputs[name] = cloneBinding(binding)
		}
	}

	if definition.Policy.DefaultOutputs != nil {
		definition.Policy.DefaultOutputs = cloneValues(definition.Policy.DefaultOutputs)
	}

	return definition
}

func validIdentifier(value string) bool {
	return identifierPattern.MatchString(value)
}

func invalidDefinitionf(format string, values ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidDefinition, fmt.Sprintf(format, values...))
}
