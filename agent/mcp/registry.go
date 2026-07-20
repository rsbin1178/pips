package agentmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
)

// RegistrySource is the narrow, pull-based MCP contract managed by a
// [Registry]. Client satisfies this interface. A Registry never reconnects or
// owns transports: applications retain credential, transport, and retry
// authority, then call Refresh when they choose.
type RegistrySource interface {
	Tools(context.Context) ([]agent.Tool, error)
	ToolListChanged() <-chan struct{}
}

// RegistryServer identifies one connected MCP server and its policy risk.
type RegistryServer struct {
	ID     string
	Source RegistrySource
	Risk   catalog.Risk
}

// RegistrySnapshot is an immutable point-in-time set of catalog entries.
// Version advances only after a complete, materially changed refresh.
type RegistrySnapshot struct {
	Version   uint64
	UpdatedAt time.Time
	Entries   []catalog.Entry
}

// Registry builds atomic, versioned MCP tool snapshots from connected
// servers. Failed refreshes leave the previous snapshot untouched. It is safe
// for concurrent use and creates no goroutines; callers decide when to refresh
// and how to handle reconnection.
type Registry struct {
	servers []RegistryServer

	mu        sync.RWMutex
	snapshot  RegistrySnapshot
	signature string
	changed   chan struct{}
}

// NewRegistry validates server identities and returns an empty registry.
func NewRegistry(servers ...RegistryServer) (*Registry, error) {
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		if server.ID == "" || server.Source == nil {
			return nil, errors.New("agent/mcp: registry server requires ID and source")
		}

		if server.Risk > catalog.RiskPrivileged {
			return nil, fmt.Errorf("agent/mcp: registry server %q has invalid risk", server.ID)
		}

		if _, exists := seen[server.ID]; exists {
			return nil, fmt.Errorf("agent/mcp: duplicate registry server %q", server.ID)
		}

		seen[server.ID] = struct{}{}
	}

	return &Registry{servers: slices.Clone(servers), changed: make(chan struct{}, 1)}, nil
}

// Refresh obtains every configured server's tools and atomically installs the
// resulting catalog entries. On error it preserves the previous snapshot.
func (r *Registry) Refresh(ctx context.Context) (RegistrySnapshot, error) {
	if r == nil {
		return RegistrySnapshot{}, errors.New("agent/mcp: nil registry")
	}

	entries := make([]catalog.Entry, 0)

	for _, server := range r.servers {
		tools, err := server.Source.Tools(ctx)
		if err != nil {
			return r.Snapshot(), fmt.Errorf("agent/mcp: refresh %q: %w", server.ID, err)
		}

		entries = append(entries, catalog.MCP(server.ID, server.Risk, tools...)...)
	}

	if _, err := catalog.New(entries...); err != nil {
		return r.Snapshot(), fmt.Errorf("agent/mcp: invalid registry snapshot: %w", err)
	}

	signature, err := entriesSignature(entries)
	if err != nil {
		return r.Snapshot(), err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if signature == r.signature {
		return cloneRegistrySnapshot(r.snapshot), nil
	}

	r.signature = signature

	r.snapshot = RegistrySnapshot{
		Version:   r.snapshot.Version + 1,
		UpdatedAt: time.Now().UTC(),
		Entries:   cloneEntries(entries),
	}
	select {
	case r.changed <- struct{}{}:
	default:
	}

	return cloneRegistrySnapshot(r.snapshot), nil
}

// RefreshChanged consumes any pending source list-change notifications. When
// none are pending it returns the current snapshot and false. A true result
// means a refresh was attempted; callers still receive an error if any source
// failed, with the prior snapshot preserved.
func (r *Registry) RefreshChanged(ctx context.Context) (RegistrySnapshot, bool, error) {
	if r == nil {
		return RegistrySnapshot{}, false, errors.New("agent/mcp: nil registry")
	}

	for _, server := range r.servers {
		select {
		case <-server.Source.ToolListChanged():
			snapshot, err := r.Refresh(ctx)
			return snapshot, true, err
		default:
		}
	}

	return r.Snapshot(), false, nil
}

// Snapshot returns the last successfully installed immutable snapshot.
func (r *Registry) Snapshot() RegistrySnapshot {
	if r == nil {
		return RegistrySnapshot{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return cloneRegistrySnapshot(r.snapshot)
}

// Changed receives a coalesced signal after a materially changed successful
// refresh. It is not closed.
func (r *Registry) Changed() <-chan struct{} {
	if r == nil {
		return nil
	}

	return r.changed
}

func entriesSignature(entries []catalog.Entry) (string, error) {
	declarations := make([]any, 0, len(entries))
	for _, entry := range entries {
		declarations = append(declarations, struct {
			Name   string
			Source catalog.Source
			Risk   catalog.Risk
			Decl   any
		}{entry.Tool.Decl().Name, entry.Source, entry.Risk, entry.Tool.Decl()})
	}

	data, err := json.Marshal(declarations)
	if err != nil {
		return "", fmt.Errorf("agent/mcp: encode registry snapshot: %w", err)
	}

	return string(data), nil
}

func cloneRegistrySnapshot(snapshot RegistrySnapshot) RegistrySnapshot {
	snapshot.Entries = cloneEntries(snapshot.Entries)

	return snapshot
}

func cloneEntries(entries []catalog.Entry) []catalog.Entry {
	cloned := slices.Clone(entries)
	for i := range cloned {
		cloned[i].Tags = slices.Clone(cloned[i].Tags)
	}

	return cloned
}
