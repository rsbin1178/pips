package mcp

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFailureCauseClassifiesOnlyByErrorType(t *testing.T) {
	t.Parallel()

	remote := &managedServer{transport: TransportStreamableHTTP, connectTimeout: 10 * time.Second}
	local := &managedServer{transport: TransportStdio, connectTimeout: 1500 * time.Millisecond}
	proxy := &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("dial 10.0.0.1:7890: refused")}
	rejected := &httpObservation{}
	rejected.status.Store(http.StatusForbidden)

	recovered := &httpObservation{}
	recovered.status.Store(http.StatusOK)

	tests := []struct {
		name     string
		err      error
		server   *managedServer
		resource any
		want     string
	}{
		{
			name: "proxy", err: fmt.Errorf("post: %w", proxy), server: remote,
			want: "the HTTP proxy could not be reached; check HTTPS_PROXY",
		},
		{
			name: "remote timeout", err: context.DeadlineExceeded, server: remote,
			want: "timed out after 10s; check network access or HTTPS_PROXY (see pips doctor)",
		},
		{name: "stdio timeout", err: context.DeadlineExceeded, server: local, want: "timed out after 1.5s"},
		{
			name: "dns", err: fmt.Errorf("post: %w", &net.DNSError{Err: "no such host", Name: "secret.example"}),
			server: remote, want: "the server host could not be resolved",
		},
		{
			name: "tls", err: fmt.Errorf("post: %w", x509.UnknownAuthorityError{}), server: remote,
			want: "the TLS handshake failed",
		},
		{
			name: "tls eof", err: fmt.Errorf("post: %w", io.EOF), server: remote,
			want: "the connection was reset; check the proxy or network",
		},
		{
			name:   "connection reset",
			err:    fmt.Errorf("post: %w", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}),
			server: remote,
			want:   "the connection was reset; check the proxy or network",
		},
		{
			name: "broken pipe", err: fmt.Errorf("post: %w", syscall.EPIPE), server: remote,
			want: "the connection was reset; check the proxy or network",
		},
		{
			name: "forbidden", err: errors.New("Forbidden"), server: remote, resource: rejected,
			want: "the server rejected the credentials (HTTP 403)",
		},
		{name: "negotiated status", err: errors.New("closed"), server: remote, resource: recovered},
		{name: "unknown", err: errors.New("secret detail"), server: remote},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, failureCause(test.err, test.server, test.resource))
		})
	}

	assert.Equal(t, "base", failureMessage("base", errors.New("secret detail"), remote, nil))
	assert.Equal(t, "base: timed out after 1.5s", failureMessage("base", context.DeadlineExceeded, local, nil))
}

func TestIsTransientConnectError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", want: false},
		{name: "eof", err: io.EOF, want: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: true},
		{name: "reset", err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}, want: true},
		{name: "broken pipe", err: syscall.EPIPE, want: true},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "cancelled", err: context.Canceled, want: false},
		{name: "permanent", err: errors.New("sentinel"), want: false},
		{name: "tls certificate", err: x509.UnknownAuthorityError{}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, isTransientConnectError(test.err))
		})
	}
}
