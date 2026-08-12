//nolint:wsl_v5 // Permission validation keeps each bounded field transition explicit.
package runtimecontrol

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"

	"github.com/rsbin1178/pips/internal/coding/config"
)

// PermissionFilesystemState describes one effective/configured filesystem
// profile and its safe provenance labels.
type PermissionFilesystemState struct {
	Effective        config.SandboxMode
	Configured       config.SandboxMode
	EffectiveSource  config.SourceKind
	ConfiguredSource config.SourceKind
	Overridden       bool
}

// PermissionNetworkState describes the retained sandbox Network setting. The
// setting remains observable while Full Access is active, but is not enforced
// by that profile; SandboxPermissionState.NetworkEnforced carries that fact.
type PermissionNetworkState struct {
	Effective        config.SandboxNetworkMode
	Configured       config.SandboxNetworkMode
	EffectiveSource  config.SourceKind
	ConfiguredSource config.SourceKind
	Overridden       bool
}

// PermissionApprovalState describes approval independently from Sandbox
// filesystem and Network permissions.
type PermissionApprovalState struct {
	Effective        config.ApprovalMode
	Configured       config.ApprovalMode
	EffectiveSource  config.SourceKind
	ConfiguredSource config.SourceKind
	Overridden       bool
}

// SandboxPermissionState is the nested process-local Sandbox projection used
// by permission frontends. Config remains backward-compatible and flat.
type SandboxPermissionState struct {
	Filesystem      PermissionFilesystemState
	Network         PermissionNetworkState
	NetworkEnforced bool
}

// PermissionState describes effective and configured process-local execution
// permissions. The legacy flat fields remain as compatibility aliases until
// all frontends consume SandboxProfile and ApprovalPolicy.
type PermissionState struct {
	// Deprecated compatibility aliases. New consumers should use the nested
	// SandboxProfile and ApprovalPolicy projections below.
	Sandbox            config.SandboxMode
	ConfiguredSandbox  config.SandboxMode
	SandboxSource      config.SourceKind
	SandboxOverridden  bool
	Approval           config.ApprovalMode
	ConfiguredApproval config.ApprovalMode
	ApprovalSource     config.SourceKind
	ApprovalOverridden bool
	Network            config.SandboxNetworkMode
	ConfiguredNetwork  config.SandboxNetworkMode
	NetworkSource      config.SourceKind
	NetworkOverridden  bool
	NetworkEnforced    bool

	SandboxProfile SandboxPermissionState
	ApprovalPolicy PermissionApprovalState
}

// SandboxPermissionUpdate is the nested Sandbox update accepted by the
// Controller boundary. Nil values retain the current effective value.
type SandboxPermissionUpdate struct {
	Filesystem *config.SandboxMode
	Network    *config.SandboxNetworkMode
}

// PermissionUpdate is one bounded process-local execution-permission change.
// SandboxProfile is the preferred nested form; the flat fields are retained
// for compatibility with the first /permissions implementation.
type PermissionUpdate struct {
	SandboxProfile *SandboxPermissionUpdate
	Sandbox        *config.SandboxMode
	Approval       *config.ApprovalMode
	Network        *config.SandboxNetworkMode
}

// FullAccessConfirmation is an opaque, one-shot capability issued by the
// Controller after the user has confirmed an exact Full Access target. Its
// fields are intentionally private so model/tool input cannot forge one.
type FullAccessConfirmation struct {
	controller        *Controller
	runtimeGeneration uint64
	sessionID         string
	targetDigest      [sha256.Size]byte
	consumed          bool
}

// SetPermissions applies one permission update by replacing the current
// Runtime at an idle boundary. It never writes configuration files or Session
// history. A Full Access transition requires the opaque confirmation issued by
// NewFullAccessConfirmation; the variadic form preserves callers that only
// update sandboxed profiles.
func (c *Controller) SetPermissions(
	ctx context.Context,
	update PermissionUpdate,
	confirmations ...*FullAccessConfirmation,
) error {
	if len(confirmations) > 1 {
		return fmt.Errorf("%w: at most one Full Access confirmation is allowed", ErrInvalid)
	}

	current, err := c.beginReplacement(ctx)
	if err != nil {
		return err
	}

	base := c.configuredConfig()
	target, err := permissionTarget(current.config, base, update)
	if err != nil {
		c.finishReplacement(current)

		return err
	}

	confirmation := firstConfirmation(confirmations)
	needsConfirmation := target.Sandbox == config.SandboxFullAccess &&
		current.config.Sandbox != config.SandboxFullAccess
	if confirmation != nil && !needsConfirmation {
		c.finishReplacement(current)

		return fmt.Errorf("%w: Full Access confirmation does not match the target", ErrInvalid)
	}
	if needsConfirmation {
		if err := c.consumeFullAccessConfirmation(current, target, confirmation); err != nil {
			c.finishReplacement(current)

			return err
		}
	}

	if current.state.IsSessionProvisional() && target.Equal(current.config) {
		c.finishReplacement(current)

		return nil
	}

	targetID := current.sessionID
	if current.state.IsSessionProvisional() {
		targetID = ""
	}

	return c.reopenReplacement(
		ctx, current, targetID, target, current.selection,
		current.resolved, current.model, current.overridden,
		false,
	)
}

// NewFullAccessConfirmation issues a target-bound, one-shot capability for a
// user-confirmed Full Access transition. It performs no Runtime replacement.
// The caller must show its own explicit warning and call SetPermissions with
// the returned capability only after the user confirms that warning.
func (c *Controller) NewFullAccessConfirmation(
	ctx context.Context,
	update PermissionUpdate,
) (*FullAccessConfirmation, error) {
	if c == nil {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if c.replacing || c.active != 0 || c.runtime == nil {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	current := c.effective.Clone()
	base := c.base.Config.Clone()
	generation := c.runtimeGeneration
	sessionID := c.sessionID
	c.mu.Unlock()

	target, err := permissionTarget(current, base, update)
	if err != nil {
		return nil, err
	}
	if current.Sandbox == config.SandboxFullAccess || target.Sandbox != config.SandboxFullAccess {
		return nil, fmt.Errorf("%w: target is not a Full Access elevation", ErrInvalid)
	}

	return &FullAccessConfirmation{
		controller:        c,
		runtimeGeneration: generation,
		sessionID:         sessionID,
		targetDigest:      permissionDigest(target),
	}, nil
}

// Permissions returns the effective and configured permission profile.
func (c *Controller) Permissions() PermissionState {
	if c == nil {
		return PermissionState{}
	}

	c.mu.Lock()
	current := c.effective.Clone()
	configured := c.base.Config.Clone()
	c.mu.Unlock()

	filesystem := permissionFilesystemState(current, configured)
	network := permissionNetworkState(current, configured)
	approval := permissionApprovalState(current, configured)
	profile := SandboxPermissionState{
		Filesystem:      filesystem,
		Network:         network,
		NetworkEnforced: current.Sandbox != config.SandboxFullAccess,
	}

	return PermissionState{
		Sandbox:            filesystem.Effective,
		ConfiguredSandbox:  filesystem.Configured,
		SandboxSource:      filesystem.EffectiveSource,
		SandboxOverridden:  filesystem.Overridden,
		Approval:           approval.Effective,
		ConfiguredApproval: approval.Configured,
		ApprovalSource:     approval.EffectiveSource,
		ApprovalOverridden: approval.Overridden,
		Network:            network.Effective,
		ConfiguredNetwork:  network.Configured,
		NetworkSource:      network.EffectiveSource,
		NetworkOverridden:  network.Overridden,
		NetworkEnforced:    profile.NetworkEnforced,
		SandboxProfile:     profile,
		ApprovalPolicy:     approval,
	}
}

func (c *Controller) configuredConfig() config.Config {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.base.Config.Clone()
}

func permissionFilesystemState(current, configured config.Config) PermissionFilesystemState {
	effectiveSource := sourceKind(current, config.FieldSandbox)
	configuredSource := sourceKind(configured, config.FieldSandbox)

	return PermissionFilesystemState{
		Effective:        current.Sandbox,
		Configured:       configured.Sandbox,
		EffectiveSource:  effectiveSource,
		ConfiguredSource: configuredSource,
		Overridden:       permissionOverridden(current.Sandbox != configured.Sandbox, effectiveSource),
	}
}

func permissionNetworkState(current, configured config.Config) PermissionNetworkState {
	effectiveSource := sourceKind(current, config.FieldSandboxNetwork)
	configuredSource := sourceKind(configured, config.FieldSandboxNetwork)

	return PermissionNetworkState{
		Effective:        current.SandboxWorkspaceWrite.Network,
		Configured:       configured.SandboxWorkspaceWrite.Network,
		EffectiveSource:  effectiveSource,
		ConfiguredSource: configuredSource,
		Overridden: permissionOverridden(
			current.SandboxWorkspaceWrite.Network != configured.SandboxWorkspaceWrite.Network,
			effectiveSource,
		),
	}
}

func permissionApprovalState(current, configured config.Config) PermissionApprovalState {
	effectiveSource := sourceKind(current, config.FieldApproval)
	configuredSource := sourceKind(configured, config.FieldApproval)

	return PermissionApprovalState{
		Effective:        current.Approval,
		Configured:       configured.Approval,
		EffectiveSource:  effectiveSource,
		ConfiguredSource: configuredSource,
		Overridden:       permissionOverridden(current.Approval != configured.Approval, effectiveSource),
	}
}

func permissionOverridden(valueChanged bool, source config.SourceKind) bool {
	return valueChanged || source == config.SourceSessionOverride
}

func sourceKind(value config.Config, field config.Field) config.SourceKind {
	source, ok := value.Source(field)
	if !ok {
		return ""
	}

	return source.Kind
}

type permissionValues struct {
	filesystem *config.SandboxMode
	network    *config.SandboxNetworkMode
	approval   *config.ApprovalMode
}

func permissionTarget(current, configured config.Config, update PermissionUpdate) (config.Config, error) {
	values, err := permissionValuesFromUpdate(update)
	if err != nil {
		return config.Config{}, err
	}

	target := current.Clone()
	if target, err = applyFilesystemPermission(target, current, configured, values.filesystem); err != nil {
		return config.Config{}, err
	}
	if target, err = applyApprovalPermission(target, current, configured, values.approval); err != nil {
		return config.Config{}, err
	}
	if target, err = applyNetworkPermission(target, current, configured, values.network); err != nil {
		return config.Config{}, err
	}
	if err := target.ValidateRuntime(); err != nil {
		return config.Config{}, fmt.Errorf("%w: permissions: %w", ErrInvalid, err)
	}

	return target, nil
}

func permissionValuesFromUpdate(update PermissionUpdate) (permissionValues, error) {
	values := permissionValues{
		filesystem: update.Sandbox,
		network:    update.Network,
		approval:   update.Approval,
	}
	if update.SandboxProfile == nil {
		return values, nil
	}

	if values.filesystem != nil && update.SandboxProfile.Filesystem != nil &&
		*values.filesystem != *update.SandboxProfile.Filesystem {
		return permissionValues{}, fmt.Errorf("%w: conflicting sandbox filesystem updates", ErrInvalid)
	}
	if values.network != nil && update.SandboxProfile.Network != nil &&
		*values.network != *update.SandboxProfile.Network {
		return permissionValues{}, fmt.Errorf("%w: conflicting sandbox Network updates", ErrInvalid)
	}
	if values.filesystem == nil {
		values.filesystem = update.SandboxProfile.Filesystem
	}
	if values.network == nil {
		values.network = update.SandboxProfile.Network
	}

	return values, nil
}

func applyFilesystemPermission(
	target, current, configured config.Config,
	value *config.SandboxMode,
) (config.Config, error) {
	if value == nil {
		return target, nil
	}

	mode, err := config.ParseSandboxMode(string(*value))
	if err != nil {
		return config.Config{}, fmt.Errorf("%w: sandbox filesystem: %w", ErrInvalid, err)
	}
	target.Sandbox = mode
	if mode == current.Sandbox {
		return target, nil
	}

	returnToFullAccess := mode == config.SandboxFullAccess && current.Sandbox != config.SandboxFullAccess
	return restoreOrOverride(
		target,
		configured,
		config.FieldSandbox,
		mode != configured.Sandbox || returnToFullAccess,
	), nil
}

func applyApprovalPermission(
	target, current, configured config.Config,
	value *config.ApprovalMode,
) (config.Config, error) {
	if value == nil {
		return target, nil
	}

	mode, err := config.ParseApprovalMode(string(*value))
	if err != nil {
		return config.Config{}, fmt.Errorf("%w: approval: %w", ErrInvalid, err)
	}
	target.Approval = mode
	if mode == current.Approval {
		return target, nil
	}

	return restoreOrOverride(target, configured, config.FieldApproval, mode != configured.Approval), nil
}

func applyNetworkPermission(
	target, current, configured config.Config,
	value *config.SandboxNetworkMode,
) (config.Config, error) {
	if value == nil {
		return target, nil
	}

	mode, err := config.ParseSandboxNetworkMode(string(*value))
	if err != nil {
		return config.Config{}, fmt.Errorf("%w: sandbox Network: %w", ErrInvalid, err)
	}
	target.SandboxWorkspaceWrite.Network = mode
	if mode == current.SandboxWorkspaceWrite.Network {
		return target, nil
	}

	return restoreOrOverride(
		target, configured, config.FieldSandboxNetwork,
		mode != configured.SandboxWorkspaceWrite.Network,
	), nil
}

func restoreOrOverride(value, configured config.Config, field config.Field, overridden bool) config.Config {
	if overridden {
		return value.WithSessionOverride(field)
	}

	return value.RestoreSourceFrom(configured, field)
}

func firstConfirmation(values []*FullAccessConfirmation) *FullAccessConfirmation {
	if len(values) == 0 {
		return nil
	}

	return values[0]
}

func (c *Controller) consumeFullAccessConfirmation(
	current replacement,
	target config.Config,
	confirmation *FullAccessConfirmation,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if confirmation == nil || confirmation.controller != c || confirmation.consumed ||
		confirmation.runtimeGeneration != current.generation ||
		confirmation.runtimeGeneration != c.runtimeGeneration ||
		confirmation.sessionID != current.sessionID ||
		confirmation.sessionID != c.sessionID ||
		confirmation.targetDigest != permissionDigest(target) {
		return fmt.Errorf("%w: Full Access confirmation is missing, stale, or for another target", ErrInvalid)
	}
	confirmation.consumed = true

	return nil
}

func permissionDigest(value config.Config) [sha256.Size]byte {
	digest := sha256.New()
	writePermissionDigest(digest, value)

	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))

	return result
}

func writePermissionDigest(digest hash.Hash, value config.Config) {
	_, _ = fmt.Fprintf(digest, "sandbox\x00%s\x00", value.Sandbox)
	_, _ = fmt.Fprintf(digest, "network\x00%s\x00", value.SandboxWorkspaceWrite.Network)
	_, _ = fmt.Fprintf(digest, "approval\x00%s\x00", value.Approval)
	for _, field := range []config.Field{
		config.FieldSandbox, config.FieldSandboxNetwork, config.FieldApproval,
	} {
		source, ok := value.Source(field)
		if !ok {
			_, _ = fmt.Fprint(digest, string(field), "\x00missing\x00")
			continue
		}
		_, _ = fmt.Fprintf(digest, "%s\x00%s\x00%s\x00", field, source.Kind, source.Detail)
	}
}
