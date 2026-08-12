//nolint:wsl_v5 // Safe library derivation keeps generation ownership and redaction checks adjacent.
package coding

import (
	"context"
	"fmt"
	"slices"

	"github.com/rsbin1178/pips/internal/coding/agentprofile"
)

// AgentLibrary is the content-safe, user-facing view of the immutable Agent
// registry. It deliberately excludes definition instructions and output JSON
// Schema: those are execution inputs, not library metadata.
type AgentLibrary struct {
	Entries []AgentLibraryEntry
}

// Clone returns a detached library snapshot.
func (l AgentLibrary) Clone() AgentLibrary {
	cloned := AgentLibrary{Entries: make([]AgentLibraryEntry, len(l.Entries))}
	for index := range l.Entries {
		cloned.Entries[index] = l.Entries[index].clone()
	}

	return cloned
}

// AgentLibraryEntry is a content-safe discovery and availability projection.
// DeclaredTools and RequiredTools are requests made by the definition; they
// are not an authority grant. Effective capabilities are captured in the
// durable child execution plan when a child is actually admitted.
type AgentLibraryEntry struct {
	ID            string
	Kind          string
	Scope         string
	Source        string
	Digest        string
	Status        string
	SuppressedBy  string
	Name          string
	Description   string
	Model         string
	UserVisible   bool
	ModelVisible  bool
	Delivery      []string
	DeclaredTools []string
	RequiredTools []string
	ToolSearch    bool
	Skills        []string
	Preloaded     []string
	OutputFormat  string
	Available     bool
	Unavailable   string
	Diagnostics   []AgentLibraryDiagnostic
}

// AgentLibraryDiagnostic is a bounded, content-free profile diagnostic.
type AgentLibraryDiagnostic struct {
	Code    string
	Source  string
	Message string
}

func (e AgentLibraryEntry) clone() AgentLibraryEntry {
	e.Delivery = slices.Clone(e.Delivery)
	e.DeclaredTools = slices.Clone(e.DeclaredTools)
	e.RequiredTools = slices.Clone(e.RequiredTools)
	e.Skills = slices.Clone(e.Skills)
	e.Preloaded = slices.Clone(e.Preloaded)
	e.Diagnostics = slices.Clone(e.Diagnostics)

	return e
}

// ListAgentProfiles returns the current registry as safe Library metadata.
// It takes a short immutable-generation lease so a simultaneous reload cannot
// clear or replace the registry while this view is being derived.
func (r *Runtime) ListAgentProfiles(ctx context.Context) (AgentLibrary, error) {
	if r == nil {
		return AgentLibrary{}, ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return AgentLibrary{}, err
	}

	r.mu.Lock()
	if r.closed || r.closing || r.integration == nil {
		r.mu.Unlock()

		return AgentLibrary{}, ErrRuntimeClosed
	}
	integration := r.integration
	if err := integration.acquire(); err != nil {
		r.mu.Unlock()

		return AgentLibrary{}, err
	}
	dynamicEnabled := r.config.DynamicSubagents
	resolver, resolverErr := newChildModelResolver(
		r.modelCatalog,
		r.resolved,
		r.model,
		r.requestPolicy,
		r.credentials,
		nil,
	)
	r.mu.Unlock()
	defer func() { _ = integration.release(context.WithoutCancel(ctx)) }()
	if resolverErr != nil {
		return AgentLibrary{}, resolverErr
	}

	return buildAgentLibrary(integration.agentProfilesSnapshot(), dynamicEnabled, resolver), nil
}

func buildAgentLibrary(
	registry agentprofile.Registry,
	dynamicEnabled bool,
	models childModelResolver,
) AgentLibrary {
	entries := registry.Entries()
	library := AgentLibrary{Entries: make([]AgentLibraryEntry, 0, len(entries))}
	for _, entry := range entries {
		library.Entries = append(library.Entries, buildAgentLibraryEntry(entry, dynamicEnabled, models))
	}

	return library.Clone()
}

func buildAgentLibraryEntry(
	entry agentprofile.Entry,
	dynamicEnabled bool,
	models childModelResolver,
) AgentLibraryEntry {
	definition := entry.Definition
	value := AgentLibraryEntry{
		ID:           entry.ID,
		Kind:         string(entry.Kind),
		Scope:        string(entry.Scope),
		Source:       entry.Source,
		Digest:       entry.Digest,
		Status:       string(entry.Status),
		SuppressedBy: entry.SuppressedBy,
		Name:         definition.Name,
		Description:  definition.Description,
		Model:        definition.Model,
		UserVisible:  definition.Visibility.User,
		ModelVisible: definition.Visibility.Model,
		ToolSearch:   definition.Tools.ToolSearch,
		OutputFormat: string(definition.Output.Format),
	}
	for _, delivery := range definition.Delivery {
		value.Delivery = append(value.Delivery, string(delivery))
	}
	for _, selector := range definition.Tools.Allow {
		value.DeclaredTools = append(value.DeclaredTools, selector.String())
	}
	for _, selector := range definition.Tools.Require {
		value.RequiredTools = append(value.RequiredTools, selector.String())
	}
	value.Skills = slices.Clone(definition.Skills.Allow)
	value.Preloaded = slices.Clone(definition.Skills.Preload)
	for _, diagnostic := range entry.Diagnostics {
		value.Diagnostics = append(value.Diagnostics, AgentLibraryDiagnostic{
			Code: diagnostic.Code, Source: diagnostic.Source, Message: diagnostic.Message,
		})
	}

	switch {
	case entry.Status != agentprofile.EntryAvailable:
		value.Unavailable = string(entry.Status)
	case definition.Kind == agentprofile.KindCustom && !dynamicEnabled:
		value.Unavailable = "dynamic subagents is disabled"
	case !definition.Visibility.User:
		value.Unavailable = "not user-invocable"
	case definition.Kind == agentprofile.KindCustom:
		if _, err := models.planModel(definition.Model); err != nil {
			// The error may contain a configured model reference. Keep the UI
			// diagnostic intentionally coarse; detailed resolution remains in
			// the child admission error and durable plan audit.
			value.Unavailable = "configured model is unavailable"
		}
	}
	value.Available = value.Unavailable == ""

	return value.clone()
}

// ValidateAgentLibrary is retained as a narrow invariant check for callers
// that cache Library data across UI updates.
func ValidateAgentLibrary(library AgentLibrary) error {
	for _, entry := range library.Entries {
		if entry.ID == "" || entry.Status == "" || entry.Kind == "" || entry.Scope == "" {
			return fmt.Errorf("%w: incomplete Agent Library entry", ErrRuntimeInvalid)
		}
	}

	return nil
}
