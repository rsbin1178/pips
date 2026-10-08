//go:build darwin || linux

package execution

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPreferSinkFailure pins the rule: the caller's failing sink is the reason the
// run stopped, so it leads the returned error even when a context error was recorded
// first, and nothing is dropped.
func TestPreferSinkFailure(t *testing.T) {
	t.Parallel()

	sinkErr := errors.New("sink stopped")
	deadline := context.DeadlineExceeded

	assert.Equal(t, deadline, preferSinkFailure(deadline, nil))
	assert.Equal(t, sinkErr, preferSinkFailure(nil, sinkErr))
	assert.Equal(t, sinkErr, preferSinkFailure(sinkErr, sinkErr))

	joined := preferSinkFailure(deadline, sinkErr)
	require.ErrorIs(t, joined, sinkErr)
	require.ErrorIs(t, joined, deadline)
	assert.Equal(t, sinkErr.Error(), strings.SplitN(joined.Error(), "\n", 2)[0], "the sink failure leads")
}

// TestRunnerPrefersTheSinkFailureOverCallerCancellation pins the same rule through
// the runner: a caller that cancels while its sink is still running, and a sink that
// then reports its own failure, still get their failure back as the reason the run
// stopped. Which of the two the runner observed first is a schedule, and the answer
// must not depend on it.
func TestRunnerPrefersTheSinkFailureOverCallerCancellation(t *testing.T) {
	t.Parallel()

	fixture := newExecutorFixture(t)
	executor := fixture.executor(t, unavailableBackend{}, systemRunnerDependencies())
	operation, authorization := fixture.fullAccessOperation(t, fixture.operationSpec("printf hello; /bin/sleep 10"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	sinkErr := errors.New("sink stopped late")
	started := make(chan struct{})

	var once sync.Once

	go func() {
		<-started
		cancel()
	}()

	result, err := executor.Execute(ctx, operation, authorization, SinkFunc(func(sinkCtx context.Context, _ OutputChunk) error {
		once.Do(func() { close(started) })

		// The cancellation reaches the runner first, then this failure lands while it
		// is draining the dispatcher.
		<-sinkCtx.Done()
		time.Sleep(10 * time.Millisecond)

		return sinkErr
	}))

	require.ErrorIs(t, err, sinkErr)
	require.ErrorIs(t, err, context.Canceled, "the cancellation is still reported")
	assert.Equal(t, StatusCanceled, result.Status)
}
