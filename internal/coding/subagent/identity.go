//nolint:wsl_v5,gocyclo // Execution-plan validation keeps immutable identity and authority checks adjacent.
package subagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/agentprofile"
)

const (
	// AgentIdentitySchema identifies the durable, non-secret child identity
	// snapshot written by the Coding subagent protocol.
	AgentIdentitySchema = "pips.coding.subagent.identity/v1alpha1"
	// ExecutionPlanSchema identifies the immutable execution snapshot consumed
	// by Manager. A plan is compiled by Coding; a definition is never executed
	// directly by this package.
	ExecutionPlanSchema = "pips.coding.subagent.plan/v1alpha1"

	maxAgentIdentityTextBytes = 4 << 10
	maxPlanInstructionsBytes  = 128 << 10
	maxPlanCapabilities       = 512
	maxPlanSkills             = 512
	maxPlanDelegationTargets  = 128
	maxPlanPrivateBindings    = 128
	maxOutputSchemaBytes      = 64 << 10
	// MaxDelegationDepth is the product hard bound for recursive custom Agent
	// edges below a root child.
	MaxDelegationDepth = 3
)

// AgentKind identifies the durable source class of one child agent.
type AgentKind string

const (
	// AgentKindBuiltin identifies a program-owned compatibility agent.
	AgentKindBuiltin AgentKind = "builtin"
	// AgentKindCustom identifies a loaded user or trusted-project definition.
	AgentKindCustom AgentKind = "custom"
	// AgentKindEphemeral identifies an explicit, non-persisted one-shot
	// definition.
	AgentKindEphemeral AgentKind = "ephemeral"
)

// AgentIdentity is the stable, non-secret identity of an execution. ID is
// canonical and must never be reconstructed from Name. Definition fields are
// a snapshot, so inspecting an old child does not depend on its source file
// still existing.
type AgentIdentity struct {
	Schema           string    `json:"schema"`
	ID               string    `json:"id"`
	Kind             AgentKind `json:"kind"`
	Name             string    `json:"name"`
	DefinitionSchema string    `json:"definition_schema"`
	DefinitionDigest string    `json:"definition_digest"`
	DefinitionSource string    `json:"definition_source"`
}

// IsZero reports whether no identity was supplied. It exists only for legacy
// journal and event decoding; new execution admission never accepts it.
func (i AgentIdentity) IsZero() bool {
	return i == (AgentIdentity{})
}

// LegacyRole returns the builtin role projection for this identity. Custom
// and ephemeral identities intentionally have no synthetic role.
func (i AgentIdentity) LegacyRole() Role {
	if i.Kind != AgentKindBuiltin {
		return ""
	}

	switch Role(i.ID) {
	case RoleExplore, RolePlan, RoleReview:
		return Role(i.ID)
	default:
		return ""
	}
}

// BuiltinIdentity returns the registry-equivalent identity for one legacy
// specialist role.
func BuiltinIdentity(role Role) (AgentIdentity, error) {
	for _, definition := range agentprofile.BuiltinDefinitions() {
		if definition.ID != string(role) {
			continue
		}

		return identityFromDefinition(definition)
	}

	return AgentIdentity{}, fmt.Errorf("%w: unknown builtin agent %q", ErrInvalid, role)
}

// IdentityFromDefinition converts one validated registry definition into the
// durable identity used by a compiled execution plan.
func IdentityFromDefinition(definition agentprofile.Definition) (AgentIdentity, error) {
	return identityFromDefinition(definition.Clone())
}

func identityFromDefinition(definition agentprofile.Definition) (AgentIdentity, error) {
	var kind AgentKind
	switch definition.Kind {
	case agentprofile.KindBuiltin:
		kind = AgentKindBuiltin
	case agentprofile.KindCustom:
		kind = AgentKindCustom
	case agentprofile.KindEphemeral:
		kind = AgentKindEphemeral
	default:
		return AgentIdentity{}, fmt.Errorf("%w: invalid definition kind %q", ErrInvalid, definition.Kind)
	}

	identity := AgentIdentity{
		Schema:           AgentIdentitySchema,
		ID:               definition.ID,
		Kind:             kind,
		Name:             definition.Name,
		DefinitionSchema: definition.Schema,
		DefinitionDigest: definition.Digest,
		DefinitionSource: definition.Source,
	}
	if err := validateIdentityBase(identity); err != nil {
		return AgentIdentity{}, err
	}

	return identity, nil
}

// ValidateIdentity validates the bounded, non-secret identity contract.
func ValidateIdentity(identity AgentIdentity) error {
	if err := validateIdentityBase(identity); err != nil {
		return err
	}
	if identity.Kind != AgentKindBuiltin && isBuiltinAgentID(identity.ID) {
		return fmt.Errorf("%w: reserved builtin agent id", ErrInvalid)
	}

	if role := identity.LegacyRole(); role != "" {
		expected, err := builtinIdentityUnchecked(role)
		if err != nil || identity != expected {
			return fmt.Errorf("%w: builtin identity does not match its reserved role", ErrInvalid)
		}
	}

	return nil
}

func isBuiltinAgentID(value string) bool {
	switch Role(value) {
	case RoleExplore, RolePlan, RoleReview:
		return true
	default:
		return false
	}
}

func validateIdentityBase(identity AgentIdentity) error {
	if identity.Schema != AgentIdentitySchema || !validAgentID(identity.ID) ||
		!validAgentKind(identity.Kind) ||
		!validIdentityText(identity.Name, 256, true) ||
		!validIdentityText(identity.DefinitionSchema, 256, true) ||
		!validSHA256(identity.DefinitionDigest) ||
		!validIdentityText(identity.DefinitionSource, maxAgentIdentityTextBytes, true) {
		return fmt.Errorf("%w: invalid agent identity", ErrInvalid)
	}

	return nil
}

func builtinIdentityUnchecked(role Role) (AgentIdentity, error) {
	for _, definition := range agentprofile.BuiltinDefinitions() {
		if definition.ID != string(role) {
			continue
		}

		return identityFromDefinition(definition)
	}

	return AgentIdentity{}, fmt.Errorf("%w: unknown builtin agent %q", ErrInvalid, role)
}

func validAgentKind(value AgentKind) bool {
	return value == AgentKindBuiltin || value == AgentKindCustom || value == AgentKindEphemeral
}

func validAgentID(value string) bool {
	if value == "" || len(value) > 64 || strings.HasPrefix(value, "-") ||
		strings.HasSuffix(value, "-") || strings.Contains(value, "--") {
		return false
	}
	for _, current := range value {
		if current == '-' || current >= 'a' && current <= 'z' || current >= '0' && current <= '9' {
			continue
		}

		return false
	}

	return true
}

func validIdentityText(value string, maximum int, required bool) bool {
	if required && value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, current := range value {
		if current == 0 || unicode.IsControl(current) {
			return false
		}
	}

	return strings.TrimSpace(value) == value
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)

	return err == nil && strings.ToLower(value) == value
}

// EffectiveCapability is one exact, runtime-owned Tool descriptor frozen into
// an execution plan. It is descriptive only; invoking the capability still
// requires a child-scoped factory and every normal execution-time policy gate.
type EffectiveCapability struct {
	WireName string `json:"wire_name"`
	Source   string `json:"source"`
	Risk     string `json:"risk"`
}

// OutputFormat identifies the local final-result contract selected by a
// compiled plan.
type OutputFormat string

const (
	// OutputFormatBuiltin delegates validation to one of the existing typed
	// builtin result adapters.
	OutputFormatBuiltin OutputFormat = "builtin"
	// OutputFormatText accepts a bounded UTF-8 text final answer.
	OutputFormatText OutputFormat = "text"
	// OutputFormatJSONSchema requires a locally validated JSON Schema result.
	OutputFormatJSONSchema OutputFormat = "json_schema"
)

// OutputContract is a frozen output-validation contract. Schema is canonical
// JSON for JSON Schema and builtin adapters, and is copied on every boundary.
type OutputContract struct {
	Format OutputFormat    `json:"format"`
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Digest string          `json:"digest"`
}

// PrivateBinding freezes one Agent-private executable resource selected from
// the immutable Coding integration generation.
type PrivateBinding struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

// Clone returns a detached output contract.
func (c OutputContract) Clone() OutputContract {
	c.Schema = slices.Clone(c.Schema)

	return c
}

// ExecutionPlan is the immutable, auditable input accepted by subagent
// execution. It records the authority intersection computed by Coding, not
// merely the agent definition's requested capabilities.
type ExecutionPlan struct {
	Schema             string                `json:"schema"`
	Identity           AgentIdentity         `json:"identity"`
	GenerationID       uint64                `json:"generation_id,omitempty"`
	Delivery           Delivery              `json:"delivery"`
	Model              string                `json:"model"`
	Instructions       string                `json:"instructions"`
	InstructionsDigest string                `json:"instructions_digest"`
	Capabilities       []EffectiveCapability `json:"capabilities"`
	Skills             []string              `json:"skills"`
	PreloadedSkills    []string              `json:"preloaded_skills"`
	ToolSearch         bool                  `json:"tool_search,omitempty"`
	DelegationDepth    int                   `json:"delegation_depth,omitempty"`
	MaxDelegationDepth int                   `json:"max_delegation_depth,omitempty"`
	Ancestry           []string              `json:"ancestry,omitempty"`
	DelegationTargets  []string              `json:"delegation_targets,omitempty"`
	PrivateMCP         []PrivateBinding      `json:"private_mcp,omitempty"`
	PrivateHooks       []PrivateBinding      `json:"private_hooks,omitempty"`
	Limits             Limits                `json:"limits"`
	Output             OutputContract        `json:"output"`
	// Legacy records did not carry an execution snapshot. It is set only while
	// normalizing those durable records for read-only inspection; dispatch must
	// never admit a legacy plan.
	Legacy bool `json:"legacy,omitempty"`
}

// Clone returns a detached execution-plan snapshot.
func (p ExecutionPlan) Clone() ExecutionPlan {
	p.Capabilities = slices.Clone(p.Capabilities)
	p.Skills = slices.Clone(p.Skills)
	p.PreloadedSkills = slices.Clone(p.PreloadedSkills)
	p.Ancestry = slices.Clone(p.Ancestry)
	p.DelegationTargets = slices.Clone(p.DelegationTargets)
	p.PrivateMCP = slices.Clone(p.PrivateMCP)
	p.PrivateHooks = slices.Clone(p.PrivateHooks)
	p.Output = p.Output.Clone()

	return p
}

// Digest returns the SHA-256 digest of the canonical JSON plan payload.
func (p ExecutionPlan) Digest() (string, error) {
	if err := validateExecutionPlan(p, true); err != nil {
		return "", err
	}

	data, err := json.Marshal(p.Clone())
	if err != nil {
		return "", fmt.Errorf("coding subagent: encode execution plan: %w", err)
	}
	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:]), nil
}

func validateExecutionPlan(plan ExecutionPlan, allowLegacy bool) error {
	if plan.Schema != ExecutionPlanSchema || ValidateIdentity(plan.Identity) != nil ||
		(plan.Delivery != DeliveryForeground && plan.Delivery != DeliveryBackground) ||
		!validModelIdentity(plan.Model) || len(plan.Instructions) > maxPlanInstructionsBytes ||
		!validPlanInstructions(plan.Instructions) || len(plan.Capabilities) > maxPlanCapabilities ||
		len(plan.Skills) > maxPlanSkills || len(plan.PreloadedSkills) > maxPlanSkills ||
		len(plan.DelegationTargets) > maxPlanDelegationTargets ||
		len(plan.PrivateMCP) > maxPlanPrivateBindings || len(plan.PrivateHooks) > maxPlanPrivateBindings ||
		!validPlanLimits(plan.Limits) ||
		validateOutputContract(plan.Output) != nil {
		return fmt.Errorf("%w: invalid execution plan", ErrInvalid)
	}

	if plan.Legacy {
		if !allowLegacy || plan.Instructions != "" || plan.InstructionsDigest != "" ||
			len(plan.Capabilities) != 0 || len(plan.Skills) != 0 ||
			len(plan.PreloadedSkills) != 0 || plan.ToolSearch ||
			plan.DelegationDepth != 0 || plan.MaxDelegationDepth != 0 ||
			len(plan.Ancestry) != 0 || len(plan.DelegationTargets) != 0 ||
			len(plan.PrivateMCP) != 0 || len(plan.PrivateHooks) != 0 {
			return fmt.Errorf("%w: invalid legacy execution plan", ErrInvalid)
		}

		return nil
	}
	if plan.Instructions == "" || !validSHA256(plan.InstructionsDigest) ||
		plan.InstructionsDigest != digestText(plan.Instructions) {
		return fmt.Errorf("%w: execution plan instructions changed", ErrInvalid)
	}

	seenCapabilities := make(map[string]struct{}, len(plan.Capabilities))
	for _, capability := range plan.Capabilities {
		if !validIdentityText(capability.WireName, 256, true) ||
			!validIdentityText(capability.Source, maxAgentIdentityTextBytes, true) ||
			!validIdentityText(capability.Risk, 128, true) {
			return fmt.Errorf("%w: invalid effective capability", ErrInvalid)
		}
		if _, duplicate := seenCapabilities[capability.WireName]; duplicate {
			return fmt.Errorf("%w: duplicate effective capability", ErrInvalid)
		}
		seenCapabilities[capability.WireName] = struct{}{}
	}

	seenSkills := make(map[string]struct{}, len(plan.Skills))
	for _, skill := range plan.Skills {
		if !validIdentityText(skill, 256, true) {
			return fmt.Errorf("%w: invalid selected skill", ErrInvalid)
		}
		if _, duplicate := seenSkills[skill]; duplicate {
			return fmt.Errorf("%w: duplicate selected skill", ErrInvalid)
		}
		seenSkills[skill] = struct{}{}
	}
	seenPreloaded := make(map[string]struct{}, len(plan.PreloadedSkills))
	for _, skill := range plan.PreloadedSkills {
		if _, allowed := seenSkills[skill]; !allowed {
			return fmt.Errorf("%w: preloaded Skill is not allowed", ErrInvalid)
		}
		if _, duplicate := seenPreloaded[skill]; duplicate {
			return fmt.Errorf("%w: duplicate preloaded Skill", ErrInvalid)
		}
		seenPreloaded[skill] = struct{}{}
	}
	if err := validatePlanDelegation(plan); err != nil {
		return err
	}
	if err := validatePrivateBindings(plan.PrivateMCP); err != nil {
		return err
	}
	if err := validatePrivateBindings(plan.PrivateHooks); err != nil {
		return err
	}

	return nil
}

func validatePrivateBindings(values []PrivateBinding) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validPrivateBindingID(value.ID) || !validSHA256(value.Fingerprint) {
			return fmt.Errorf("%w: invalid private resource binding", ErrInvalid)
		}
		if _, duplicate := seen[value.ID]; duplicate {
			return fmt.Errorf("%w: duplicate private resource binding", ErrInvalid)
		}
		seen[value.ID] = struct{}{}
	}

	return nil
}

func validPrivateBindingID(value string) bool {
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

func validatePlanDelegation(plan ExecutionPlan) error {
	if plan.DelegationDepth == 0 && plan.MaxDelegationDepth == 0 &&
		len(plan.Ancestry) == 0 && len(plan.DelegationTargets) == 0 {
		return nil // Pre-recursion non-legacy plans remain readable and runnable.
	}
	if plan.Identity.Kind == AgentKindBuiltin || plan.DelegationDepth < 0 ||
		plan.MaxDelegationDepth < 0 || plan.MaxDelegationDepth > MaxDelegationDepth ||
		plan.DelegationDepth > plan.MaxDelegationDepth ||
		len(plan.Ancestry) != plan.DelegationDepth+1 ||
		plan.Ancestry[len(plan.Ancestry)-1] != plan.Identity.ID {
		return fmt.Errorf("%w: invalid execution plan delegation lineage", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(plan.Ancestry)+len(plan.DelegationTargets))
	for _, id := range plan.Ancestry {
		if !validAgentID(id) {
			return fmt.Errorf("%w: invalid execution plan delegation ancestry", ErrInvalid)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("%w: cyclic execution plan delegation ancestry", ErrInvalid)
		}
		seen[id] = struct{}{}
	}
	if len(plan.DelegationTargets) > 0 &&
		(plan.Delivery != DeliveryForeground || plan.DelegationDepth >= plan.MaxDelegationDepth) {
		return fmt.Errorf("%w: execution plan cannot delegate at this depth or delivery", ErrInvalid)
	}
	targets := make(map[string]struct{}, len(plan.DelegationTargets))
	for _, id := range plan.DelegationTargets {
		if !validAgentID(id) {
			return fmt.Errorf("%w: invalid execution plan delegation target", ErrInvalid)
		}
		if _, ancestor := seen[id]; ancestor {
			return fmt.Errorf("%w: execution plan delegation target is an ancestor", ErrInvalid)
		}
		if _, duplicate := targets[id]; duplicate {
			return fmt.Errorf("%w: duplicate execution plan delegation target", ErrInvalid)
		}
		targets[id] = struct{}{}
	}

	return nil
}

func validPlanInstructions(value string) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, current := range value {
		if current == 0 {
			return false
		}
	}

	return true
}

func validPlanLimits(value Limits) bool {
	return validateLimits(value) == nil
}

func validateOutputContract(contract OutputContract) error {
	if !validOutputFormat(contract.Format) || !validIdentityText(contract.Name, 256, true) ||
		len(contract.Schema) > maxOutputSchemaBytes || !validSHA256(contract.Digest) {
		return fmt.Errorf("%w: invalid output contract", ErrInvalid)
	}
	if len(contract.Schema) > 0 && !json.Valid(contract.Schema) {
		return fmt.Errorf("%w: invalid output contract schema", ErrInvalid)
	}
	if contract.Digest != digestOutputContract(contract) {
		return fmt.Errorf("%w: output contract changed", ErrInvalid)
	}

	return nil
}

func validOutputFormat(value OutputFormat) bool {
	return value == OutputFormatBuiltin || value == OutputFormatText || value == OutputFormatJSONSchema
}

func digestOutputContract(contract OutputContract) string {
	payload := struct {
		Format OutputFormat    `json:"format"`
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema,omitempty"`
	}{Format: contract.Format, Name: contract.Name, Schema: contract.Schema}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:])
}

func digestText(value string) string {
	digest := sha256.Sum256([]byte(value))

	return hex.EncodeToString(digest[:])
}

// InstructionsDigest returns the durable digest used to prove a compiled
// execution plan still carries its exact rendered instruction text.
func InstructionsDigest(value string) string { return digestText(value) }

// ValidateExecutionPlan validates a non-legacy plan at the Coding composition
// boundary. Legacy snapshots remain readable through journal inspection but
// cannot be dispatched through this API.
func ValidateExecutionPlan(plan ExecutionPlan) error {
	return validateExecutionPlan(plan, false)
}

func sameExecutionPlan(left, right ExecutionPlan) bool {
	leftDigest, leftErr := left.Digest()
	rightDigest, rightErr := right.Digest()

	return leftErr == nil && rightErr == nil && leftDigest == rightDigest
}
