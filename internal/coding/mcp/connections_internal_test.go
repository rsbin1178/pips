//nolint:paralleltest,wsl_v5 // Cooldown/backoff tests intentionally mutate package timing state.
package mcp

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

func TestRetryFailedHonorsCooldownAndRecovers(t *testing.T) {
	originalCooldown := failedRetryCooldown
	failedRetryCooldown = 0
	t.Cleanup(func() { failedRetryCooldown = originalCooldown })

	server := sdk.NewServer(&sdk.Implementation{Name: "recover", Version: "v1"}, nil)
	server.AddTool(&sdk.Tool{
		Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})

	attempts := &atomic.Int32{}
	factory := func(ctx context.Context, _ Definition) (sdk.Transport, io.Closer, error) {
		if attempts.Add(1) <= 3 {
			return &failingTransport{}, nil, nil
		}
		serverTransport, clientTransport := sdk.NewInMemoryTransports()
		session, err := server.Connect(context.WithoutCancel(ctx), serverTransport, nil)
		if err != nil {
			return nil, nil, err
		}

		return clientTransport, closeFunc(session.Close), nil
	}

	definition := Definition{
		ID: "recover", Scope: ScopeUser, Transport: TransportStreamableHTTP,
		URL: "https://recover.example.test/mcp", ConnectTimeout: 5 * time.Second,
	}
	connections, err := OpenConnections(
		context.Background(),
		[]ResolvedDefinition{{Definition: definition, Status: StatusEnabled}},
		ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16, Transport: factory,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	require.NoError(t, connections.Wait(context.Background()))
	require.Eventually(t, func() bool {
		return connections.Servers()[0].State == ServerStateFailed
	}, 30*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 3, attempts.Load())

	connections.RetryFailed(context.Background())
	require.Eventually(t, func() bool {
		return connections.Servers()[0].State == ServerStateConnected
	}, 30*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 4, attempts.Load())
	assert.Equal(t, []string{"recover_lookup"}, connections.Servers()[0].Tools)
}

func TestWatchRecoversLostSession(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "lost", Version: "v1"}, nil)
	server.AddTool(&sdk.Tool{
		Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})

	sessions := make(chan *sdk.ServerSession, 8)
	factory := func(ctx context.Context, _ Definition) (sdk.Transport, io.Closer, error) {
		serverTransport, clientTransport := sdk.NewInMemoryTransports()
		session, err := server.Connect(context.WithoutCancel(ctx), serverTransport, nil)
		if err != nil {
			return nil, nil, err
		}
		sessions <- session

		return clientTransport, closeFunc(session.Close), nil
	}

	definition := Definition{
		ID: "lost", Scope: ScopeUser, Transport: TransportStreamableHTTP,
		URL: "https://lost.example.test/mcp", ConnectTimeout: 5 * time.Second,
	}
	connections, err := OpenConnections(
		context.Background(),
		[]ResolvedDefinition{{Definition: definition, Status: StatusEnabled}},
		ConnectionOptions{
			Implementation: &sdk.Implementation{Name: "pips-test", Version: "v1"},
			MaxTools:       16, Transport: factory,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connections.Close()) })

	require.NoError(t, connections.Wait(context.Background()))
	require.Equal(t, ServerStateConnected, connections.Servers()[0].State)

	first := <-sessions
	require.NoError(t, first.Close())

	select {
	case <-sessions:
	case <-time.After(30 * time.Second):
		t.Fatal("recovery did not create a replacement session")
	}
	require.Eventually(t, func() bool {
		return connections.Servers()[0].State == ServerStateConnected
	}, 30*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"lost_lookup"}, connections.Servers()[0].Tools)
}

type failingTransport struct{}

func (*failingTransport) Connect(context.Context) (sdk.Connection, error) {
	return nil, io.EOF
}
