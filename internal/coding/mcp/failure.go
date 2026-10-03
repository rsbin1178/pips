package mcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
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

	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "the server host could not be resolved"
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return "the connection was refused"
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
