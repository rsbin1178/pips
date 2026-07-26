package coding

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeWorkspaceStatusRejectsBusyAndCanceledQueries(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("unused")))

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := runtime.WorkspaceStatus(canceled)
	require.ErrorIs(t, err, context.Canceled)

	runtime.mu.Lock()
	runtime.active = &runtimeOperation{cancel: func() {}, done: make(chan struct{})}
	runtime.mu.Unlock()
	_, err = runtime.WorkspaceStatus(t.Context())
	require.ErrorIs(t, err, ErrRuntimeBusy)
	runtime.mu.Lock()
	runtime.active = nil
	runtime.state.Phase = PhasePaused
	runtime.mu.Unlock()
	_, err = runtime.WorkspaceStatus(t.Context())
	require.ErrorIs(t, err, ErrRuntimeBusy)
	runtime.mu.Lock()
	runtime.state.Phase = PhaseIdle
	runtime.mu.Unlock()
}

func TestRuntimeWorkspaceStatusRejectsClosedRuntime(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("unused")))
	require.NoError(t, runtime.Close(t.Context()))

	_, err := runtime.WorkspaceStatus(t.Context())
	require.ErrorIs(t, err, ErrRuntimeClosed)
}
