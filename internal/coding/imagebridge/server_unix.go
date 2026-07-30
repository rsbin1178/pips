//go:build darwin || linux

package imagebridge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/rsbin/pips/internal/coding/attachment"
)

const (
	inboxCapacity       = 4
	listenerPollPeriod  = 200 * time.Millisecond
	connectionTimeout   = 5 * time.Second
	socketDirectoryMode = 0o700
	socketMode          = 0o600
)

type unixIdentity struct {
	device uint64
	inode  uint64
}

type serverDependencies struct {
	baseDirectory string
	uid           int
	now           func() time.Time
	peerUID       func(*net.UnixConn) (int, error)
}

// Server owns one nonce-scoped private Unix socket and bounded image inbox.
type Server struct {
	cancel    context.CancelFunc
	listener  *net.UnixListener
	inbox     chan attachment.Image
	done      chan struct{}
	path      string
	directory string
	identity  unixIdentity
	dirID     unixIdentity
	created   bool
	deps      serverDependencies
	closeOnce sync.Once
	closeErr  error
}

// Listen creates one process-owned endpoint under /tmp/pips-bridge-<uid>.
func Listen(ctx context.Context, nonce string) (*Server, error) {
	return listen(ctx, nonce, systemServerDependencies())
}

func systemServerDependencies() serverDependencies {
	return serverDependencies{
		baseDirectory: "/tmp",
		uid:           os.Getuid(),
		now:           time.Now,
		peerUID:       unixPeerUID,
	}
}

func listen(ctx context.Context, nonce string, deps serverDependencies) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := ValidateNonce(nonce); err != nil {
		return nil, err
	}

	if deps.baseDirectory == "" || deps.uid < 0 || deps.now == nil || deps.peerUID == nil {
		return nil, fmt.Errorf("%w: invalid listener dependencies", ErrUnavailable)
	}

	directory := filepath.Join(deps.baseDirectory, fmt.Sprintf("pips-bridge-%d", deps.uid))

	created, dirID, err := ensurePrivateDirectory(directory, deps.uid)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(directory, nonce+".sock")

	listener, identity, err := openEndpoint(path, deps.uid)
	if err != nil {
		if created {
			err = errors.Join(err, removeOwnedDirectory(directory, dirID))
		}

		return nil, err
	}

	serverCtx, cancel := context.WithCancel(ctx)
	server := &Server{
		cancel: cancel, listener: listener, inbox: make(chan attachment.Image, inboxCapacity),
		done: make(chan struct{}), path: path, directory: directory,
		identity: identity, dirID: dirID, created: created, deps: deps,
	}

	go server.accept(serverCtx)

	return server, nil
}

func openEndpoint(path string, uid int) (*net.UnixListener, unixIdentity, error) {
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return nil, unixIdentity{}, fmt.Errorf("%w: endpoint already exists", ErrUnavailable)
	}

	address := &net.UnixAddr{Name: path, Net: "unix"}

	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, unixIdentity{}, fmt.Errorf("%w: create endpoint", ErrUnavailable)
	}

	listener.SetUnlinkOnClose(false)

	createdIdentity, _, ok := inspectIdentity(path)
	if !ok {
		_ = listener.Close()

		return nil, unixIdentity{}, fmt.Errorf("%w: inspect created endpoint", ErrUnavailable)
	}

	fail := func(primary error) (*net.UnixListener, unixIdentity, error) {
		return nil, unixIdentity{}, errors.Join(
			primary,
			listener.Close(),
			removeOwnedSocket(path, createdIdentity),
		)
	}

	if err := os.Chmod(path, socketMode); err != nil {
		return fail(fmt.Errorf("%w: secure endpoint", ErrUnavailable))
	}

	identity, err := validateSocket(path, uid)
	if err != nil {
		return fail(err)
	}

	if identity != createdIdentity {
		return fail(fmt.Errorf("%w: endpoint identity changed", ErrUnavailable))
	}

	return listener, identity, nil
}

// Receive waits for one already-validated immutable image.
func (s *Server) Receive(ctx context.Context) (attachment.Image, error) {
	if s == nil {
		return attachment.Image{}, ErrClosed
	}

	select {
	case image, ok := <-s.inbox:
		if !ok {
			return attachment.Image{}, ErrClosed
		}

		return image, nil
	case <-ctx.Done():
		return attachment.Image{}, ctx.Err()
	}
}

// Close stops admission, waits for the receiver, and removes only the socket
// inode created by this Server.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}

	s.closeOnce.Do(func() {
		s.cancel()
		s.closeErr = s.listener.Close()
		<-s.done

		s.closeErr = errors.Join(s.closeErr, removeOwnedSocket(s.path, s.identity))
		if s.created {
			s.closeErr = errors.Join(s.closeErr, removeOwnedDirectory(s.directory, s.dirID))
		}
	})

	return s.closeErr
}

func (s *Server) accept(ctx context.Context) {
	defer close(s.done)
	defer close(s.inbox)

	for {
		_ = s.listener.SetDeadline(s.deps.now().Add(listenerPollPeriod))

		connection, err := s.listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}

			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				continue
			}

			return
		}

		s.handle(ctx, connection)
	}
}

func (s *Server) handle(ctx context.Context, connection *net.UnixConn) {
	defer func() { _ = connection.Close() }()

	now := s.deps.now()
	_ = connection.SetDeadline(now.Add(connectionTimeout))

	uid, err := s.deps.peerUID(connection)
	if err != nil || uid != s.deps.uid {
		return
	}

	frame, err := Decode(ctx, connection, now)
	if err != nil || !frame.Image.Valid() || frame.Notice != NoticeUnknown {
		s.writeNotice(connection, NoticeRejected, now)

		return
	}

	notice := NoticeAccepted

	select {
	case s.inbox <- frame.Image:
	default:
		notice = NoticeBusy
	}

	s.writeNotice(connection, notice, now)
}

func (s *Server) writeNotice(connection *net.UnixConn, notice Notice, now time.Time) {
	_ = Encode(connection, Frame{Notice: notice, Deadline: now.Add(connectionTimeout)})
	_ = connection.CloseWrite()
}

// Send validates the endpoint and transfers one image to the matching live
// bridge, waiting for its bounded acknowledgement.
func Send(ctx context.Context, nonce string, frame Frame) error {
	return send(ctx, nonce, frame, systemServerDependencies())
}

func send(ctx context.Context, nonce string, frame Frame, deps serverDependencies) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := ValidateNonce(nonce); err != nil {
		return err
	}

	if !frame.Image.Valid() || frame.Notice != NoticeUnknown {
		return fmt.Errorf("%w: upload requires an image", ErrProtocol)
	}

	connection, err := connectEndpoint(ctx, nonce, deps)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()

	return exchangeFrame(ctx, connection, frame, deps.now)
}

func connectEndpoint(
	ctx context.Context,
	nonce string,
	deps serverDependencies,
) (*net.UnixConn, error) {
	directory := filepath.Join(deps.baseDirectory, fmt.Sprintf("pips-bridge-%d", deps.uid))
	if _, _, err := inspectPrivateDirectory(directory, deps.uid); err != nil {
		return nil, err
	}

	path := filepath.Join(directory, nonce+".sock")
	if _, err := validateSocket(path, deps.uid); err != nil {
		return nil, err
	}

	dialer := net.Dialer{}

	raw, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("%w: connect endpoint", ErrUnavailable)
	}

	connection, ok := raw.(*net.UnixConn)
	if !ok {
		_ = raw.Close()

		return nil, fmt.Errorf("%w: unexpected endpoint", ErrUnavailable)
	}

	uid, err := deps.peerUID(connection)
	if err != nil || uid != deps.uid {
		_ = connection.Close()

		return nil, ErrPeer
	}

	return connection, nil
}

func exchangeFrame(
	ctx context.Context,
	connection *net.UnixConn,
	frame Frame,
	now func() time.Time,
) error {
	if now == nil {
		return fmt.Errorf("%w: missing clock", ErrUnavailable)
	}

	deadline := now().Add(connectionTimeout)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}

	_ = connection.SetDeadline(deadline)

	if err := Encode(connection, frame); err != nil {
		return err
	}

	if err := connection.CloseWrite(); err != nil {
		return fmt.Errorf("%w: finish upload", ErrUnavailable)
	}

	acknowledgement, err := Decode(ctx, connection, now())
	if err != nil {
		return err
	}

	switch acknowledgement.Notice {
	case NoticeAccepted:
		return nil
	case NoticeBusy:
		return ErrBusy
	case NoticeRejected:
		return ErrProtocol
	default:
		return ErrProtocol
	}
}

func ensurePrivateDirectory(path string, uid int) (bool, unixIdentity, error) {
	if err := os.Mkdir(path, socketDirectoryMode); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return false, unixIdentity{}, fmt.Errorf("%w: create endpoint directory", ErrUnavailable)
		}

		_, identity, inspectErr := inspectPrivateDirectory(path, uid)

		return false, identity, inspectErr
	}

	_, identity, err := inspectPrivateDirectory(path, uid)

	return true, identity, err
}

func inspectPrivateDirectory(path string, uid int) (fs.FileInfo, unixIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, unixIdentity{}, fmt.Errorf("%w: inspect endpoint directory", ErrUnavailable)
	}

	identity, owner, ok := fileIdentityAndOwner(info)
	if !ok || !info.IsDir() || info.Mode().Perm() != socketDirectoryMode || owner != uid {
		return nil, unixIdentity{}, fmt.Errorf("%w: insecure endpoint directory", ErrUnavailable)
	}

	return info, identity, nil
}

func validateSocket(path string, uid int) (unixIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return unixIdentity{}, fmt.Errorf("%w: inspect endpoint", ErrUnavailable)
	}

	identity, owner, ok := fileIdentityAndOwner(info)
	if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != socketMode || owner != uid {
		return unixIdentity{}, fmt.Errorf("%w: insecure endpoint", ErrUnavailable)
	}

	return identity, nil
}

func inspectIdentity(path string) (unixIdentity, int, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return unixIdentity{}, 0, false
	}

	return fileIdentityAndOwner(info)
}

func removeOwnedSocket(path string, expected unixIdentity) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("%w: inspect endpoint for cleanup", ErrUnavailable)
	}

	identity, _, ok := fileIdentityAndOwner(info)
	if !ok || identity != expected {
		return fmt.Errorf("%w: endpoint identity changed", ErrUnavailable)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("%w: remove endpoint", ErrUnavailable)
	}

	return nil
}

func removeOwnedDirectory(path string, expected unixIdentity) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("%w: inspect endpoint directory for cleanup", ErrUnavailable)
	}

	identity, _, ok := fileIdentityAndOwner(info)
	if !ok || identity != expected {
		return fmt.Errorf("%w: endpoint directory identity changed", ErrUnavailable)
	}

	err = os.Remove(path)
	if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("%w: remove endpoint directory", ErrUnavailable)
	}

	return nil
}
