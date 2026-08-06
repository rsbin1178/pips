//nolint:wsl_v5,paralleltest // Process lifecycle tests intentionally launch a helper child.
package pluginsupervisor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakePreLaunchController struct {
	closeErr   error
	closeCalls int
}

func (*fakePreLaunchController) configure(*exec.Cmd) error { return nil }
func (*fakePreLaunchController) attach(*exec.Cmd) error    { return nil }
func (*fakePreLaunchController) terminate(context.Context, *exec.Cmd, <-chan struct{}) (processTermination, error) {
	return processTermination{}, nil
}

func (c *fakePreLaunchController) close() error {
	c.closeCalls++
	return c.closeErr
}

type failingPreLaunchCloser struct {
	err   error
	calls int
}

func (c *failingPreLaunchCloser) Close() error {
	c.calls++
	return c.err
}

type quiescentAttachController struct{}

func (*quiescentAttachController) configure(*exec.Cmd) error { return nil }
func (*quiescentAttachController) attach(*exec.Cmd) error    { return nil }
func (*quiescentAttachController) terminate(context.Context, *exec.Cmd, <-chan struct{}) (processTermination, error) {
	return processTermination{treeQuiesced: true}, nil
}
func (*quiescentAttachController) close() error { return nil }

//nolint:paralleltest // This test owns a helper process and must remain serialized.
func TestCleanupFailedAttachKillsUnassignedDirectChild(t *testing.T) {
	// This models the Windows attach race where the Job Object is empty because
	// OpenProcess/AssignProcessToJobObject failed before ownership was attached.
	// The direct child must still be terminated even when the controller reports
	// an apparently quiescent tree.
	cmd := exec.CommandContext(context.Background(), os.Args[0]) //nolint:gosec // The test helper is the current test binary.
	env := append(os.Environ(), "PLUGIN_SUPERVISOR_HELPER=1", "PLUGIN_SUPERVISOR_MODE=no-bootstrap")
	cmd.Env = env
	require.NoError(t, cmd.Start())

	canRemoveRoot, err := cleanupFailedAttach(
		&quiescentAttachController{}, cmd, io.NopCloser(nil), io.NopCloser(nil), 10*time.Millisecond,
	)
	require.True(t, canRemoveRoot)
	require.NoError(t, err)
}

func TestFailPreLaunchControllerCloseFailureRetainsRoot(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("controller close failed")
	controller := &fakePreLaunchController{closeErr: closeErr}

	canRemoveRoot, err := failPreLaunch(
		"test.plugin",
		errors.New("start failed"),
		controller,
	)

	require.False(t, canRemoveRoot)
	require.Equal(t, 1, controller.closeCalls)
	require.ErrorIs(t, err, ErrLaunch)
	require.ErrorIs(t, err, closeErr)
	require.ErrorIs(t, err, ErrCleanupUncertain)
	require.NotContains(t, err.Error(), "controller close failed")

	var processErr *ProcessError
	require.ErrorAs(t, err, &processErr)
	require.Equal(t, FailureLaunch, processErr.Kind)
}

func TestFailPreLaunchClosesResourcesAndAllowsRootRemoval(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("pipe close failed")
	closer := &failingPreLaunchCloser{err: closeErr}
	controller := &fakePreLaunchController{}

	canRemoveRoot, err := failPreLaunch(
		"test.plugin",
		errors.New("pipe setup failed"),
		controller,
		closer,
	)

	require.True(t, canRemoveRoot)
	require.Equal(t, 1, closer.calls)
	require.Equal(t, 1, controller.closeCalls)
	require.ErrorIs(t, err, ErrLaunch)
	require.ErrorIs(t, err, closeErr)
	require.NotErrorIs(t, err, ErrCleanupUncertain)
}
