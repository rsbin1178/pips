//nolint:wsl_v5 // Retry fixtures keep factory, wait, and assertion steps adjacent.
package mcp_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flakyTransport fails the first `fail` connects with a transient error, then
// serves one in-memory MCP session.
type flakyTransport struct {
	server   *sdk.Server
	attempts *atomic.Int32
	fail     int32
	session  *sdk.ServerSession
}

func (t *flakyTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	if t.attempts.Add(1) <= t.fail {
		return nil, io.EOF
	}

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	session, err := t.server.Connect(context.WithoutCancel(ctx), serverTransport, nil)
	if err != nil {
		return nil, err
	}
	t.session = session

	return clientTransport.Connect(ctx)
}

func (t *flakyTransport) Close() error {
	if t.session == nil {
		return nil
	}

	return t.session.Close()
}

func TestConnectionsRetryTransientConnectFailures(t *testing.T) {
	t.Parallel()

	server := testMCPServer("flaky", "lookup")
	attempts := &atomic.Int32{}
	started := time.Now()
	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{enabledHTTPDefinition("flaky")},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16,
			Transport: func(context.Context, codingmcp.Definition) (sdk.Transport, io.Closer, error) {
				transport := &flakyTransport{server: server, attempts: attempts, fail: 2}

				return transport, transport, nil
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	require.NoError(t, connections.Wait(t.Context()))
	assert.GreaterOrEqual(t, time.Since(started), 1500*time.Millisecond)
	assert.EqualValues(t, 3, attempts.Load())
	statuses := connections.Servers()
	require.Len(t, statuses, 1)
	assert.Equal(t, codingmcp.ServerStateConnected, statuses[0].State)
	assert.Equal(t, []string{"flaky_lookup"}, statuses[0].Tools)
}

func TestConnectionsDoNotRetryPermanentConnectFailures(t *testing.T) {
	t.Parallel()

	attempts := &atomic.Int32{}
	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{enabledHTTPDefinition("permanent")},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16,
			Transport: func(context.Context, codingmcp.Definition) (sdk.Transport, io.Closer, error) {
				return &terminalTransport{attempts: attempts, err: errors.New("sentinel auth failure")}, nil, nil
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	require.NoError(t, connections.Wait(t.Context()))
	assert.EqualValues(t, 1, attempts.Load())
	diagnostics := connections.TakeDiagnostics()
	require.Len(t, diagnostics, 1)
	assert.NotContains(t, diagnostics[0].Message, "sentinel")
}

func TestConnectionsDoNotRetryAfterConnectBudgetExpires(t *testing.T) {
	t.Parallel()

	attempts := &atomic.Int32{}
	definition := enabledHTTPDefinition("slow")
	definition.Definition.ConnectTimeout = 100 * time.Millisecond
	connections, err := codingmcp.OpenConnections(
		t.Context(),
		[]codingmcp.ResolvedDefinition{definition},
		codingmcp.ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16,
			Transport:      blockingTransport(attempts),
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	require.NoError(t, connections.Wait(t.Context()))
	assert.EqualValues(t, 1, attempts.Load())
	diagnostics := connections.TakeDiagnostics()
	require.Len(t, diagnostics, 1)
	assert.Contains(t, diagnostics[0].Message, "timed out after 100ms")
}

type terminalTransport struct {
	attempts *atomic.Int32
	err      error
}

func (t *terminalTransport) Connect(context.Context) (sdk.Connection, error) {
	t.attempts.Add(1)

	return nil, t.err
}

func blockingTransport(attempts *atomic.Int32) codingmcp.TransportFactory {
	return func(context.Context, codingmcp.Definition) (sdk.Transport, io.Closer, error) {
		return &blockingMCPTransport{attempts: attempts}, nil, nil
	}
}

type blockingMCPTransport struct {
	attempts *atomic.Int32
}

func (t *blockingMCPTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	t.attempts.Add(1)
	<-ctx.Done()

	return nil, ctx.Err()
}
