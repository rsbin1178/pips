//nolint:wsl_v5 // Snapshot projection keeps the lease and release steps adjacent.
package coding

import (
	"context"
	"errors"
	"slices"

	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
)

// MCPSnapshot is the content-free MCP connection projection for frontends. It
// reflects the currently published integration generation only.
type MCPSnapshot struct {
	GenerationID uint64                   `json:"generation_id"`
	Settled      bool                     `json:"settled"`
	Servers      []codingmcp.ServerStatus `json:"servers"`
}

// Clone returns a defensive copy.
func (s MCPSnapshot) Clone() MCPSnapshot {
	s.Servers = slices.Clone(s.Servers)
	for index := range s.Servers {
		s.Servers[index].Tools = slices.Clone(s.Servers[index].Tools)
	}

	return s
}

// MCP returns per-server connection state for the published generation
// without exposing definitions, credentials, or transports. Unlike Skills it
// stays readable during an interaction: the interaction's lease pins the same
// generation, so no mixed view is possible.
func (r *Runtime) MCP(ctx context.Context) (_ MCPSnapshot, returnErr error) {
	if r == nil {
		return MCPSnapshot{}, ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return MCPSnapshot{}, err
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return MCPSnapshot{}, stateError("mcp", phase, ErrRuntimeClosed)
	}
	integration := r.integration
	if integration == nil {
		r.mu.Unlock()
		return MCPSnapshot{}, ErrRuntimeClosed
	}
	if err := integration.acquire(); err != nil {
		r.mu.Unlock()
		return MCPSnapshot{}, err
	}
	r.mu.Unlock()
	defer func() {
		returnErr = errors.Join(
			returnErr,
			integration.release(context.WithoutCancel(ctx)),
		)
	}()

	connections := integration.connectionsSnapshot()

	return MCPSnapshot{
		GenerationID: integration.ID(),
		Settled:      connections.Settled(),
		Servers:      connections.Servers(),
	}, nil
}
