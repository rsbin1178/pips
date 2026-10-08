package httpx

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveStreamIdleTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero selects the default", 0, DefaultStreamIdleTimeout},
		{"negative selects the default", -time.Second, DefaultStreamIdleTimeout},
		{"explicit value wins", 45 * time.Second, 45 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, resolveStreamIdleTimeout(tc.in))
		})
	}
}

// stallBody hands out queued chunks and then blocks until it is closed, which
// is the shape of a connection that stays open but stops delivering bytes.
type stallBody struct {
	chunks chan []byte
	done   chan struct{}
	once   sync.Once
}

func newStallBody() *stallBody {
	return &stallBody{chunks: make(chan []byte, 64), done: make(chan struct{})}
}

func (b *stallBody) push(s string) { b.chunks <- []byte(s) }

func (b *stallBody) Read(p []byte) (int, error) {
	select {
	case chunk := <-b.chunks:
		return copy(p, chunk), nil
	case <-b.done:
		return 0, errors.New("stallBody: closed")
	}
}

func (b *stallBody) Close() error {
	b.once.Do(func() { close(b.done) })

	return nil
}

func TestIdleReadCloserAbortsASilentRead(t *testing.T) {
	t.Parallel()

	body := newStallBody()

	reader := newIdleReadCloser(body, 40*time.Millisecond)
	defer func() { _ = reader.Close() }()

	buf := make([]byte, 16)

	_, err := reader.Read(buf)

	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrStreamIdle, "the abort has to be the retryable failure class")
	assert.Contains(t, err.Error(), ai.ErrStreamIdle.Error(), "the abort names the class")
}

func TestIdleReadCloserDeliversWithinTheWindow(t *testing.T) {
	t.Parallel()

	const (
		bound = 100 * time.Millisecond
		gap   = 10 * time.Millisecond
		reads = 15
	)

	body := newStallBody()

	reader := newIdleReadCloser(body, bound)
	defer func() { _ = reader.Close() }()

	buf := make([]byte, 16)
	start := time.Now()

	for i := range reads {
		body.push("x")

		n, err := reader.Read(buf)
		require.NoErrorf(t, err, "read %d", i)
		assert.Equal(t, 1, n)

		time.Sleep(gap)
	}

	// The bound is measured per read, never as a total: this stream ran for
	// longer than the bound and every read still succeeded.
	assert.Greater(t, time.Since(start), bound, "the stream outlived the bound without being aborted")
}

func TestIdleReadCloserCloseStopsTheBound(t *testing.T) {
	t.Parallel()

	body := newStallBody()
	reader := newIdleReadCloser(body, 40*time.Millisecond)

	require.NoError(t, reader.Close())

	buf := make([]byte, 16)

	_, err := reader.Read(buf)

	require.Error(t, err, "a closed stream does not deliver")
	assert.NotErrorIs(t, err, ai.ErrStreamIdle, "a closed stream is not a silent stream")
}

func TestNewIdleReadCloserWithoutABoundReturnsTheBody(t *testing.T) {
	t.Parallel()

	body := newStallBody()
	defer func() { _ = body.Close() }()

	// The caller resolves zero to the default before reaching this
	// constructor, so a non-positive bound here means the stream is unguarded.
	got, ok := newIdleReadCloser(body, 0).(*stallBody)

	assert.True(t, ok, "an unguarded stream returns the original body")
	assert.Same(t, body, got)
}

func TestIdleReadCloserCloseWinsOverTheBound(t *testing.T) {
	t.Parallel()

	body := newStallBody()
	reader := newIdleReadCloser(body, time.Hour)

	defer func() { _ = reader.Close() }()

	// A read parked far inside the bound is released by Close, and the close is
	// what it reports: a stream the caller closed is not a silent stream.
	parked := make(chan error, 1)

	go func() {
		_, err := reader.Read(make([]byte, 16))
		parked <- err
	}()

	time.Sleep(20 * time.Millisecond)

	require.NoError(t, reader.Close())

	select {
	case err := <-parked:
		require.Error(t, err)
		require.NotErrorIs(t, err, ai.ErrStreamIdle)
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not release a parked read")
	}
}
