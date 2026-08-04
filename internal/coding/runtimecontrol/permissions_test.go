//nolint:wsl_v5 // Permission replacement cases keep setup and assertions adjacent.
package runtimecontrol

import (
	"testing"

	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/config"
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
	assert.Equal(t, config.SourceDefault, state.ApprovalSource)
	assert.Equal(t, config.SourceDefault, state.NetworkSource)
	assert.True(t, state.ApprovalOverridden)
	assert.True(t, state.NetworkOverridden)
	assert.Equal(t, config.ApprovalNever, controller.Config().Approval)
	assert.Equal(t, config.SandboxNetworkDeny, controller.Config().SandboxWorkspaceWrite.Network)

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

func TestControllerPermissionFullAccessCannotBeElevatedOrRestored(t *testing.T) {
	t.Parallel()

	normal := newControllerFixture(t)
	controller, err := newController(t.Context(), normal.options, normal.dependencies())
	require.NoError(t, err)
	full := config.SandboxFullAccess
	err = controller.SetPermissions(t.Context(), PermissionUpdate{Sandbox: &full})
	require.Error(t, err)
	require.ErrorContains(t, err, "Full Access elevation")
	assert.Len(t, normal.opener.calls, 1)
	assert.Equal(t, 0, normal.opener.runtimes[0].closeCalls())
	require.NoError(t, controller.Close(t.Context()))

	narrow := newControllerFixture(t)
	narrow.options.Config.Sandbox = config.SandboxFullAccess
	controller, err = newController(t.Context(), narrow.options, narrow.dependencies())
	require.NoError(t, err)
	workspace := config.SandboxWorkspaceWrite
	require.NoError(t, controller.SetPermissions(t.Context(), PermissionUpdate{Sandbox: &workspace}))
	assert.Equal(t, config.SandboxWorkspaceWrite, controller.Permissions().Sandbox)
	assert.Empty(t, narrow.opener.calls[1].Session.ID)

	err = controller.SetPermissions(t.Context(), PermissionUpdate{Sandbox: &full})
	require.Error(t, err)
	require.ErrorContains(t, err, "Full Access elevation")
	assert.Len(t, narrow.opener.calls, 2)
	require.NoError(t, controller.Close(t.Context()))
}
