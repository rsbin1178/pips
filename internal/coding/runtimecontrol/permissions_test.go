//nolint:wsl_v5 // Permission replacement cases keep setup and assertions adjacent.
package runtimecontrol

import (
	"testing"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerPermissionUpdateReopensSameSessionAndPreservesProvenance(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	fixture.opener.runtimes[0].setState(func(state *coding.State) {
		state.Interaction.ID = "interaction-1"
	})
	initialID := controller.SessionID()

	approval := config.ApprovalNever
	network := config.SandboxNetworkDeny
	require.NoError(t, controller.SetPermissions(t.Context(), PermissionUpdate{
		Approval: &approval,
		Network:  &network,
	}))

	assert.Equal(t, initialID, controller.SessionID())
	require.Len(t, fixture.opener.calls, 2)
	assert.Equal(t, initialID, fixture.opener.calls[1].Session.ID)
	assert.Equal(t, approval, fixture.opener.calls[1].Config.Approval)
	assert.Equal(t, network, fixture.opener.calls[1].Config.SandboxWorkspaceWrite.Network)

	state := controller.Permissions()
	assert.Equal(t, config.ApprovalNever, state.Approval)
	assert.Equal(t, config.SandboxNetworkDeny, state.Network)
	assert.Equal(t, config.ApprovalOnRequest, state.ConfiguredApproval)
	assert.Equal(t, config.SandboxNetworkOnRequest, state.ConfiguredNetwork)
	assert.Equal(t, config.SourceSessionOverride, state.ApprovalSource)
	assert.Equal(t, config.SourceSessionOverride, state.NetworkSource)
	assert.Equal(t, config.SourceSessionOverride, state.ApprovalPolicy.EffectiveSource)
	assert.Equal(t, config.SourceSessionOverride, state.SandboxProfile.Network.EffectiveSource)
	assert.True(t, state.ApprovalOverridden)
	assert.True(t, state.NetworkOverridden)
	assert.Equal(t, config.ApprovalNever, controller.Config().Approval)
	assert.Equal(t, config.SandboxNetworkDeny, controller.Config().SandboxWorkspaceWrite.Network)

	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerPermissionProfilesRetainNetworkAndRestoreSources(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	fixture.opener.runtimes[0].setState(func(state *coding.State) {
		state.Interaction.ID = "interaction-1"
	})

	readOnly := config.SandboxReadOnly
	network := config.SandboxNetworkAllow
	require.NoError(t, controller.SetPermissions(t.Context(), PermissionUpdate{
		SandboxProfile: &SandboxPermissionUpdate{Filesystem: &readOnly, Network: &network},
	}))
	state := controller.Permissions()
	assert.Equal(t, readOnly, state.SandboxProfile.Filesystem.Effective)
	assert.Equal(t, network, state.SandboxProfile.Network.Effective)
	assert.True(t, state.SandboxProfile.NetworkEnforced)
	assert.Equal(t, config.SourceSessionOverride, state.SandboxProfile.Filesystem.EffectiveSource)
	assert.Equal(t, config.SourceSessionOverride, state.SandboxProfile.Network.EffectiveSource)

	full := config.SandboxFullAccess
	fullUpdate := PermissionUpdate{SandboxProfile: &SandboxPermissionUpdate{Filesystem: &full}}
	confirmation, err := controller.NewFullAccessConfirmation(t.Context(), fullUpdate)
	require.NoError(t, err)
	require.NoError(t, controller.SetPermissions(t.Context(), fullUpdate, confirmation))
	state = controller.Permissions()
	assert.False(t, state.SandboxProfile.NetworkEnforced)
	assert.Equal(t, network, state.SandboxProfile.Network.Effective)
	assert.Equal(t, config.SourceSessionOverride, state.SandboxProfile.Network.EffectiveSource)

	workspace := config.SandboxWorkspaceWrite
	require.NoError(t, controller.SetPermissions(t.Context(), PermissionUpdate{
		SandboxProfile: &SandboxPermissionUpdate{Filesystem: &workspace},
	}))
	state = controller.Permissions()
	assert.True(t, state.SandboxProfile.NetworkEnforced)
	assert.Equal(t, network, state.SandboxProfile.Network.Effective)
	assert.Equal(t, config.SourceDefault, state.SandboxProfile.Filesystem.EffectiveSource)
	assert.Equal(t, config.SourceSessionOverride, state.SandboxProfile.Network.EffectiveSource)

	onRequest := config.SandboxNetworkOnRequest
	require.NoError(t, controller.SetPermissions(t.Context(), PermissionUpdate{
		SandboxProfile: &SandboxPermissionUpdate{Network: &onRequest},
	}))
	state = controller.Permissions()
	assert.Equal(t, config.SourceDefault, state.SandboxProfile.Network.EffectiveSource)
	assert.False(t, state.SandboxProfile.Network.Overridden)
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerPermissionConfirmationIsTargetBoundAndOneShot(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	fixture.opener.runtimes[0].setState(func(state *coding.State) {
		state.Interaction.ID = "interaction-1"
	})
	full := config.SandboxFullAccess
	update := PermissionUpdate{Sandbox: &full}
	confirmation, err := controller.NewFullAccessConfirmation(t.Context(), update)
	require.NoError(t, err)

	wrongTarget := config.SandboxNetworkDeny
	err = controller.SetPermissions(t.Context(), PermissionUpdate{
		SandboxProfile: &SandboxPermissionUpdate{Filesystem: &full, Network: &wrongTarget},
	}, confirmation)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Len(t, fixture.opener.calls, 1)
	assert.Equal(t, 0, fixture.opener.runtimes[0].closeCalls())

	require.NoError(t, controller.SetPermissions(t.Context(), update, confirmation))
	err = controller.SetPermissions(t.Context(), update, confirmation)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Len(t, fixture.opener.calls, 2)
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerPermissionConfirmationRejectsWrongOwnerAndStaleGeneration(t *testing.T) {
	t.Parallel()

	ownerFixture := newControllerFixture(t)
	owner, err := newController(t.Context(), ownerFixture.options, ownerFixture.dependencies())
	require.NoError(t, err)
	ownerFixture.opener.runtimes[0].setState(func(state *coding.State) {
		state.Interaction.ID = "interaction-1"
	})

	full := config.SandboxFullAccess
	update := PermissionUpdate{Sandbox: &full}
	confirmation, err := owner.NewFullAccessConfirmation(t.Context(), update)
	require.NoError(t, err)

	otherFixture := newControllerFixture(t)
	other, err := newController(t.Context(), otherFixture.options, otherFixture.dependencies())
	require.NoError(t, err)
	err = other.SetPermissions(t.Context(), update, confirmation)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Equal(t, config.SandboxWorkspaceWrite, other.Permissions().Sandbox)
	assert.Len(t, otherFixture.opener.calls, 1)
	require.NoError(t, owner.SetPermissions(t.Context(), update, confirmation))

	staleFixture := newControllerFixture(t)
	stale, err := newController(t.Context(), staleFixture.options, staleFixture.dependencies())
	require.NoError(t, err)
	staleFixture.opener.runtimes[0].setState(func(state *coding.State) {
		state.Interaction.ID = "interaction-1"
	})
	staleConfirmation, err := stale.NewFullAccessConfirmation(t.Context(), update)
	require.NoError(t, err)
	require.NoError(t, stale.NewSession(t.Context()))
	err = stale.SetPermissions(t.Context(), update, staleConfirmation)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Equal(t, config.SandboxWorkspaceWrite, stale.Permissions().Sandbox)

	require.NoError(t, owner.Close(t.Context()))
	require.NoError(t, other.Close(t.Context()))
	require.NoError(t, stale.Close(t.Context()))
}

func TestControllerPermissionFullAccessRollbackConsumesConfirmation(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	fixture.opener.failures[1] = assert.AnError
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	fixture.opener.runtimes[0].setState(func(state *coding.State) {
		state.Interaction.ID = "interaction-1"
	})
	full := config.SandboxFullAccess
	update := PermissionUpdate{Sandbox: &full}
	confirmation, err := controller.NewFullAccessConfirmation(t.Context(), update)
	require.NoError(t, err)

	err = controller.SetPermissions(t.Context(), update, confirmation)
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, config.SandboxWorkspaceWrite, controller.Permissions().Sandbox)
	assert.Equal(t, 1, fixture.opener.runtimes[0].closeCalls())

	err = controller.SetPermissions(t.Context(), update, confirmation)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Len(t, fixture.opener.calls, 3)
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerPermissionNoOpDoesNotReplaceRuntime(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)

	approval := config.ApprovalOnRequest
	network := config.SandboxNetworkOnRequest
	require.NoError(t, controller.SetPermissions(t.Context(), PermissionUpdate{
		Approval: &approval,
		Network:  &network,
	}))

	require.Len(t, fixture.opener.runtimes, 1)
	assert.Equal(t, 0, fixture.opener.runtimes[0].closeCalls())
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerPermissionReplacementRollsBackOnTargetOpenFailure(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t)
	targetErr := assert.AnError
	fixture.opener.failures[1] = targetErr
	controller, err := newController(t.Context(), fixture.options, fixture.dependencies())
	require.NoError(t, err)
	fixture.opener.runtimes[0].setState(func(state *coding.State) {
		state.Interaction.ID = "interaction-1"
	})
	initialID := controller.SessionID()
	approval := config.ApprovalNever

	err = controller.SetPermissions(t.Context(), PermissionUpdate{Approval: &approval})
	require.ErrorIs(t, err, targetErr)
	assert.False(t, controller.Detached())
	assert.Equal(t, initialID, controller.SessionID())
	assert.Equal(t, config.ApprovalOnRequest, controller.Permissions().Approval)
	require.Len(t, fixture.opener.calls, 3)
	assert.Equal(t, initialID, fixture.opener.calls[1].Session.ID)
	assert.Equal(t, initialID, fixture.opener.calls[2].Session.ID)
	require.NoError(t, controller.Close(t.Context()))
}

func TestControllerPermissionFullAccessRequiresConfirmationAndCanBeRestored(t *testing.T) {
	t.Parallel()

	normal := newControllerFixture(t)
	controller, err := newController(t.Context(), normal.options, normal.dependencies())
	require.NoError(t, err)
	initialID := controller.SessionID()
	normal.opener.runtimes[0].setState(func(state *coding.State) {
		state.Interaction.ID = "interaction-1"
	})
	full := config.SandboxFullAccess
	err = controller.SetPermissions(t.Context(), PermissionUpdate{Sandbox: &full})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Len(t, normal.opener.calls, 1)
	assert.Equal(t, 0, normal.opener.runtimes[0].closeCalls())

	update := PermissionUpdate{Sandbox: &full}
	confirmation, err := controller.NewFullAccessConfirmation(t.Context(), update)
	require.NoError(t, err)
	require.NoError(t, controller.SetPermissions(t.Context(), update, confirmation))
	assert.Equal(t, full, controller.Permissions().Sandbox)
	assert.Equal(t, initialID, controller.SessionID())
	assert.Equal(t, config.SourceSessionOverride, controller.Permissions().SandboxSource)
	require.NoError(t, controller.Close(t.Context()))

	narrow := newControllerFixture(t)
	narrow.options.Config.Sandbox = config.SandboxFullAccess
	controller, err = newController(t.Context(), narrow.options, narrow.dependencies())
	require.NoError(t, err)
	workspace := config.SandboxWorkspaceWrite
	require.NoError(t, controller.SetPermissions(t.Context(), PermissionUpdate{Sandbox: &workspace}))
	assert.Equal(t, config.SandboxWorkspaceWrite, controller.Permissions().Sandbox)
	assert.Empty(t, narrow.opener.calls[1].Session.ID)

	update = PermissionUpdate{Sandbox: &full}
	confirmation, err = controller.NewFullAccessConfirmation(t.Context(), update)
	require.NoError(t, err)
	require.NoError(t, controller.SetPermissions(t.Context(), update, confirmation))
	state := controller.Permissions()
	assert.Equal(t, full, state.Sandbox)
	assert.Equal(t, config.SourceSessionOverride, state.SandboxProfile.Filesystem.EffectiveSource)
	assert.True(t, state.SandboxProfile.Filesystem.Overridden)
	assert.Len(t, narrow.opener.calls, 3)

	calls := len(narrow.opener.calls)
	require.NoError(t, controller.SetPermissions(t.Context(), update))
	assert.Len(t, narrow.opener.calls, calls)
	assert.Equal(t, config.SourceSessionOverride, controller.Permissions().SandboxProfile.Filesystem.EffectiveSource)
	require.NoError(t, controller.Close(t.Context()))
}
