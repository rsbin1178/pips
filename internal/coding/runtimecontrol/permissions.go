//nolint:wsl_v5 // Permission validation keeps each bounded field transition explicit.
package runtimecontrol

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/internal/coding/config"
)

// PermissionState describes the effective and configured process-local
// execution permissions. Source kinds are safe provenance labels; source
// details such as config paths and environment variable names stay private.
type PermissionState struct {
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
}

// PermissionUpdate is one bounded process-local execution-permission change.
// Nil fields retain the current effective value.
type PermissionUpdate struct {
	Sandbox  *config.SandboxMode
	Approval *config.ApprovalMode
	Network  *config.SandboxNetworkMode
}

// SetPermissions applies one permission update by replacing the current
// Runtime at an idle boundary. It never writes configuration files or Session
// history and never creates a Full Access grant from an ordinary session.
func (c *Controller) SetPermissions(ctx context.Context, update PermissionUpdate) error {
	current, err := c.beginReplacement(ctx)
	if err != nil {
		return err
	}

	target, err := permissionTarget(current.config, update)
	if err != nil {
		c.finishReplacement(current)

		return err
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
	)
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

	return PermissionState{
		Sandbox:            current.Sandbox,
		ConfiguredSandbox:  configured.Sandbox,
		SandboxSource:      sourceKind(configured, config.FieldSandbox),
		SandboxOverridden:  current.Sandbox != configured.Sandbox,
		Approval:           current.Approval,
		ConfiguredApproval: configured.Approval,
		ApprovalSource:     sourceKind(configured, config.FieldApproval),
		ApprovalOverridden: current.Approval != configured.Approval,
		Network:            current.SandboxWorkspaceWrite.Network,
		ConfiguredNetwork:  configured.SandboxWorkspaceWrite.Network,
		NetworkSource:      sourceKind(configured, config.FieldSandboxNetwork),
		NetworkOverridden:  current.SandboxWorkspaceWrite.Network != configured.SandboxWorkspaceWrite.Network,
	}
}

func sourceKind(value config.Config, field config.Field) config.SourceKind {
	source, ok := value.Source(field)
	if !ok {
		return ""
	}

	return source.Kind
}

func permissionTarget(current config.Config, update PermissionUpdate) (config.Config, error) {
	target := current.Clone()
	if update.Sandbox != nil {
		mode, err := config.ParseSandboxMode(string(*update.Sandbox))
		if err != nil {
			return config.Config{}, fmt.Errorf("%w: sandbox: %w", ErrInvalid, err)
		}
		if mode == config.SandboxFullAccess && current.Sandbox != config.SandboxFullAccess {
			return config.Config{}, fmt.Errorf(
				"%w: Full Access elevation is available only through explicit user configuration",
				ErrInvalid,
			)
		}
		target.Sandbox = mode
	}
	if update.Approval != nil {
		mode, err := config.ParseApprovalMode(string(*update.Approval))
		if err != nil {
			return config.Config{}, fmt.Errorf("%w: approval: %w", ErrInvalid, err)
		}
		target.Approval = mode
	}
	if update.Network != nil {
		mode, err := config.ParseSandboxNetworkMode(string(*update.Network))
		if err != nil {
			return config.Config{}, fmt.Errorf("%w: workspace network: %w", ErrInvalid, err)
		}
		target.SandboxWorkspaceWrite.Network = mode
	}
	if err := target.ValidateRuntime(); err != nil {
		return config.Config{}, fmt.Errorf("%w: permissions: %w", ErrInvalid, err)
	}

	return target, nil
}
