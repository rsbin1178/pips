package mcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"
)

// httpObservation records the status of the latest same-origin JSON-RPC POST
// response so a failed handshake can name the HTTP status without carrying
// any server-supplied text. It is the transport resource of a Streamable HTTP
// definition; closing it releases nothing.
type httpObservation struct {
	status atomic.Int32
}

func (o *httpObservation) record(request *http.Request, response *http.Response) {
	if o == nil || response == nil || request.Method != http.MethodPost {
		return
	}

	o.status.Store(int32(response.StatusCode)) //nolint:gosec // HTTP status codes are three digits.
}

func (o *httpObservation) lastStatus() int {
	if o == nil {
		return 0
	}

	return int(o.status.Load())
}

// Close implements io.Closer.
func (*httpObservation) Close() error {
	return nil
}

// failureMessage appends a fixed, credential-free cause to base. Only error
// types and the observed HTTP status are consulted; server error content and
// addresses never reach the message.
func failureMessage(base string, err error, server *managedServer, resource any) string {
	cause := failureCause(err, server, resource)
	if cause == "" {
		return base
	}

	return base + ": " + cause
}

//nolint:gocyclo // One closed classification per failure family keeps the order explicit.
func failureCause(err error, server *managedServer, resource any) string {
	remote := server.transport == TransportStreamableHTTP

	var operation *net.OpError
	if errors.As(err, &operation) && operation.Op == "proxyconnect" {
		return "the HTTP proxy could not be reached; check HTTPS_PROXY"
	}

	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		message := "timed out after " + server.connectTimeout.Round(time.Millisecond).String()
		if remote {
			message += "; check network access or HTTPS_PROXY (see pips doctor)"
		}

		return message
	}

	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return "the server host could not be resolved"
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return "the connection was refused"
	}

	if isConnectionReset(err) {
		return "the connection was reset; check the proxy or network"
	}

	if isTLSFailure(err) {
		return "the TLS handshake failed"
	}

	if observation, ok := resource.(*httpObservation); ok {
		switch status := observation.lastStatus(); {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return fmt.Sprintf("the server rejected the credentials (HTTP %d)", status)
		case status >= http.StatusBadRequest:
			return fmt.Sprintf("the server answered HTTP %d", status)
		}
	}

	return ""
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }

	return errors.As(err, &timeout) && timeout.Timeout()
}

// isConnectionReset reports a peer-side close that carries no protocol
// meaning. A proxy that drops a fresh CONNECT tunnel during TLS commonly
// surfaces as io.EOF; an established HTTP/2 stream commonly surfaces as
// ECONNRESET or EPIPE. These are the failures worth retrying.
func isConnectionReset(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE)
}

// isTransportLoss reports a tool-call failure that means the session's
// transport is gone. Unlike connect retries, a tool call may also observe
// ECONNREFUSED when the peer process or listener disappeared.
func isTransportLoss(err error) bool {
	return isConnectionReset(err) || errors.Is(err, syscall.ECONNREFUSED)
}

// isRetryableConnectError decides whether a failed server should be offered to
// the next interaction. A server that refused the connection is worth another
// attempt; a refused proxy is a configuration problem the user must fix.
func isRetryableConnectError(err error) bool {
	if isTransientConnectError(err) {
		return true
	}

	if !errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}

	var operation *net.OpError
	if errors.As(err, &operation) && operation.Op == "proxyconnect" {
		return false
	}

	return true
}

// isTransientConnectError reports whether a failed handshake may succeed on a
// fresh transport. Authentication, protocol, and configuration failures are
// deliberately excluded so a retry cannot replay a permanent decision.
func isTransientConnectError(err error) bool {
	if err == nil {
		return false
	}

	if isConnectionReset(err) {
		return true
	}

	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return true
	}

	dns, ok := errors.AsType[*net.DNSError](err)

	return ok && dns.IsTemporary
}

func isTLSFailure(err error) bool {
	var (
		verification *tls.CertificateVerificationError
		record       tls.RecordHeaderError
		alert        tls.AlertError
		authority    x509.UnknownAuthorityError
		hostname     x509.HostnameError
		invalid      x509.CertificateInvalidError
	)

	return errors.As(err, &verification) || errors.As(err, &record) || errors.As(err, &alert) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}
