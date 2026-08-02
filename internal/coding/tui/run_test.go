package tui

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResetTerminalModesDisablesMouseAndAlternateScroll(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	require.NoError(t, resetTerminalModes(&output))
	assert.Equal(t, resetTerminalInteraction, output.String())
	assert.Contains(t, output.String(), "\x1b[?1007l")
	assert.Contains(t, output.String(), "\x1b[?1002l")
	assert.Contains(t, output.String(), "\x1b[?1003l")
	assert.NotContains(t, output.String(), "\x1b[?1007h")
	assert.NotContains(t, output.String(), "\x1b[?1002h")
}

func TestCloseControllerOutlivesCanceledProgramContext(t *testing.T) {
	t.Parallel()

	want := errors.New("close failed")
	controller := &closeContextController{
		stubController: stubController{state: readyState()},
		err:            want,
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := closeController(ctx, controller)
	require.ErrorIs(t, err, want)
	assert.NoError(t, controller.contextErr)
}

func TestFinishRunExitSuppressesCallbackAfterFailureOrSignal(t *testing.T) {
	t.Parallel()

	want := errors.New("cleanup failed")
	called := 0
	handler := func(ExitInfo) error {
		called++

		return nil
	}

	require.ErrorIs(t, finishRunExit(want, true, handler, ExitInfo{}), want)
	require.NoError(t, finishRunExit(nil, false, handler, ExitInfo{}))
	assert.Zero(t, called)

	require.NoError(t, finishRunExit(nil, true, handler, ExitInfo{Resumable: true}))
	assert.Equal(t, 1, called)
}

type closeContextController struct {
	stubController
	err        error
	contextErr error
}

func (c *closeContextController) Close(ctx context.Context) error {
	c.contextErr = ctx.Err()

	return c.err
}
