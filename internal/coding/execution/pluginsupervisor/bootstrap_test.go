package pluginsupervisor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type gatedReadError struct {
	ready   chan struct{}
	release chan struct{}
	err     error
}

func (r *gatedReadError) Read([]byte) (int, error) {
	close(r.ready)
	<-r.release

	return 0, r.err
}

func TestMonitorStdoutLetsProcessExitWinReadClose(t *testing.T) {
	t.Parallel()

	reader := &gatedReadError{
		ready:   make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New("stdout closed"),
	}
	violation := make(chan error, 1)
	cancel := make(chan struct{}, 1)
	done := make(chan struct{})
	waitDone := make(chan struct{})

	go monitorStdout(reader, violation, cancel, done, waitDone)

	<-reader.ready

	close(waitDone)
	close(reader.release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stdout monitor did not stop")
	}

	_, ok := <-violation

	require.False(t, ok)
}

func TestMonitorStdoutTreatsLiveReadCloseAsProtocolViolation(t *testing.T) {
	t.Parallel()

	violation := make(chan error, 1)
	cancel := make(chan struct{}, 1)
	done := make(chan struct{})
	waitDone := make(chan struct{})

	go monitorStdout(strings.NewReader(""), violation, cancel, done, waitDone)

	select {
	case err := <-violation:
		require.ErrorIs(t, err, ErrBootstrap)
	case <-time.After(time.Second):
		t.Fatal("stdout monitor did not report a live read close")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stdout monitor did not stop")
	}

	_, ok := <-violation
	require.False(t, ok)
}
