//go:build darwin || linux

package imagebridge

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/attachment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerTransfersImagesAndBoundsInbox(t *testing.T) {
	t.Parallel()

	deps := testServerDependencies(t)
	nonce := bridgeNonceFixture(t, 7)
	server, err := listen(t.Context(), nonce, deps)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	image := bridgeImageFixture(t)
	for range inboxCapacity {
		require.NoError(t, send(t.Context(), nonce, uploadFrame(deps, image), deps))
	}

	err = send(t.Context(), nonce, uploadFrame(deps, image), deps)
	require.ErrorIs(t, err, ErrBusy)

	for range inboxCapacity {
		received, receiveErr := server.Receive(t.Context())
		require.NoError(t, receiveErr)
		assert.True(t, image.Equal(received))
	}

	require.NoError(t, send(t.Context(), nonce, uploadFrame(deps, image), deps))
}

func TestServerRefusesPreexistingAndInsecureObjects(t *testing.T) {
	t.Parallel()

	deps := testServerDependencies(t)
	nonce := bridgeNonceFixture(t, 8)
	directory := filepath.Join(deps.baseDirectory, "pips-bridge-"+strconv.Itoa(deps.uid))
	require.NoError(t, os.Mkdir(directory, socketDirectoryMode))
	require.NoError(t, os.WriteFile(filepath.Join(directory, nonce+".sock"), []byte("owned"), socketMode))

	_, err := listen(t.Context(), nonce, deps)
	require.ErrorIs(t, err, ErrUnavailable)
	assert.NotContains(t, err.Error(), nonce)
	assert.NotContains(t, err.Error(), directory)

	require.NoError(t, os.RemoveAll(directory))
	require.NoError(t, os.Mkdir(directory, 0o750))

	_, err = listen(t.Context(), nonce, deps)
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestServerCleanupRefusesReplacement(t *testing.T) {
	t.Parallel()

	deps := testServerDependencies(t)
	nonce := bridgeNonceFixture(t, 9)
	server, err := listen(t.Context(), nonce, deps)
	require.NoError(t, err)

	oldPath := server.path + ".old"
	require.NoError(t, os.Rename(server.path, oldPath))
	require.NoError(t, os.WriteFile(server.path, []byte("replacement"), socketMode))

	err = server.Close()
	require.ErrorIs(t, err, ErrUnavailable)
	assert.FileExists(t, server.path)
	require.NoError(t, os.Remove(server.path))
	require.NoError(t, os.Remove(oldPath))
}

func TestSendRejectsWrongPeerAndCancellation(t *testing.T) {
	t.Parallel()

	deps := testServerDependencies(t)
	nonce := bridgeNonceFixture(t, 10)
	server, err := listen(t.Context(), nonce, deps)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	wrongPeer := deps
	wrongPeer.peerUID = func(*net.UnixConn) (int, error) { return deps.uid + 1, nil }
	err = send(t.Context(), nonce, uploadFrame(deps, bridgeImageFixture(t)), wrongPeer)
	require.ErrorIs(t, err, ErrPeer)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	err = send(canceled, nonce, uploadFrame(deps, bridgeImageFixture(t)), deps)
	require.ErrorIs(t, err, context.Canceled)
}

func testServerDependencies(t *testing.T) serverDependencies {
	t.Helper()

	sequence := testDirectorySequence.Add(1)
	baseDirectory := filepath.Join(
		"/tmp",
		"pib-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatUint(sequence, 10),
	)
	require.NoError(t, os.Mkdir(baseDirectory, 0o700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(baseDirectory)) })

	return serverDependencies{
		baseDirectory: baseDirectory,
		uid:           os.Getuid(),
		now:           time.Now,
		peerUID:       unixPeerUID,
	}
}

var testDirectorySequence atomic.Uint64

func bridgeNonceFixture(t *testing.T, fill byte) string {
	t.Helper()

	nonce, err := newNonce(bytes.NewReader(bytes.Repeat([]byte{fill}, nonceBytes)))
	require.NoError(t, err)

	return nonce
}

func uploadFrame(deps serverDependencies, image attachment.Image) Frame {
	return Frame{Image: image, Deadline: deps.now().Add(10 * time.Second)}
}
