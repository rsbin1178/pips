// Package catalog composes explicitly registered agent tools into policy-gated
// snapshots. It deliberately does not discover executables from skill folders:
// scripts become callable only after a host wraps them as an agent.Tool or MCP
// tool and assigns provenance and risk.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/agent"
)

// Risk classifies the highest expected impact of a tool invocation.
type Risk uint8

// Tool risk levels, ordered from least to most privileged.
const (
	RiskRead Risk = iota
	RiskWrite
	RiskPrivileged
)

// Source identifies how a tool reached the application composition root.
// The catalog never imports optional MCP or team packages; their callers pass
// the snapshots through the corresponding constructor helpers below.
type Source struct {
	Kind string
	ID   string
}

const (
	// SourceLocal identifies a tool implemented by the application process.
	SourceLocal = "local"
	// SourceMCP identifies a tool from an MCP server snapshot.
	SourceMCP = "mcp"
	// SourceExtension identifies a tool supplied by an installed extension.
	SourceExtension = "extension"
	// SourceTeam identifies a Team-scoped agent tool.
	SourceTeam = "team"
)

// Entry is one explicitly registered tool and its policy metadata.
type Entry struct {
	Tool   agent.Tool
	Source Source
	Risk   Risk
	Tags   []string
}

// Descriptor is the immutable metadata visible to a policy decision.
type Descriptor struct {
	Name        string
	Description string
	Source      Source
	Risk        Risk
	Tags        []string
}

// Authorizer can make tenant-specific decisions after static allowlist and
// risk checks pass. Returning false hides the tool from the snapshot.
type Authorizer func(context.Context, string, Descriptor) (bool, error)

// Policy bounds a snapshot. It is deny-by-default: callers must provide an
// allowlist or use AllowAll deliberately. MaxRisk defaults to RiskRead.
type Policy struct {
	TenantID  string
	Allowlist []string
	MaxRisk   Risk
	Authorize Authorizer
}

// AllowAll deliberately opts into every registered tool up to maxRisk. It is
// intended for trusted single-tenant applications; multi-tenant hosts should
// supply a precise allowlist and Authorize function.
func AllowAll(tenantID string, maxRisk Risk) Policy {
	return Policy{TenantID: tenantID, Allowlist: []string{"*"}, MaxRisk: maxRisk}
}

// Catalog is immutable after construction and safe for concurrent snapshots.
type Catalog struct {
	entries []Entry
	byName  map[string]Entry
}

// New validates and indexes explicitly registered tools.
func New(entries ...Entry) (*Catalog, error) {
	catalog := &Catalog{
		entries: make([]Entry, 0, len(entries)),
		byName:  make(map[string]Entry, len(entries)),
	}
	for _, entry := range entries {
		if entry.Tool == nil {
			return nil, errors.New("catalog: nil tool")
		}

		decl := entry.Tool.Decl()
		if strings.TrimSpace(decl.Name) == "" {
			return nil, fmt.Errorf("catalog: tool with empty name (%T)", entry.Tool)
		}

		if _, exists := catalog.byName[decl.Name]; exists {
			return nil, fmt.Errorf("catalog: duplicate tool name %q", decl.Name)
		}

		if entry.Risk > RiskPrivileged {
			return nil, fmt.Errorf("catalog: tool %q has invalid risk", decl.Name)
		}

		if !validSource(entry.Source) {
			return nil, fmt.Errorf("catalog: tool %q has invalid source", decl.Name)
		}

		entry.Tags = slices.Clone(entry.Tags)
		catalog.entries = append(catalog.entries, entry)
		catalog.byName[decl.Name] = entry
	}

	return catalog, nil
}

// Merge combines immutable catalogs in argument order. It preserves each
// entry's tool, provenance, risk, and tags, and returns an independent Catalog.
// A nil input or duplicate tool name is an error; no inputs produce an empty
// Catalog.
func Merge(catalogs ...*Catalog) (*Catalog, error) {
	for i, catalog := range catalogs {
		if catalog == nil {
			return nil, fmt.Errorf("catalog: merge catalog %d is nil", i)
		}
	}

	var entries []Entry
	for _, catalog := range catalogs {
		entries = append(entries, catalog.entries...)
	}

	return New(entries...)
}

// Snapshot returns tools authorized for this tenant in registration order.
func (c *Catalog) Snapshot(ctx context.Context, policy Policy) ([]agent.Tool, error) {
	if c == nil {
		return nil, errors.New("catalog: nil catalog")
	}

	if policy.MaxRisk > RiskPrivileged {
		return nil, errors.New("catalog: invalid maximum risk")
	}

	tools := make([]agent.Tool, 0, len(c.entries))
	for _, entry := range c.entries {
		allowed, err := policy.allows(ctx, describe(entry))
		if err != nil {
			return nil, fmt.Errorf("catalog: authorize %q: %w", entry.Tool.Decl().Name, err)
		}

		if allowed {
			tools = append(tools, entry.Tool)
		}
	}

	return tools, nil
}

// Search returns policy-authorized descriptors whose name, description, source
// or tags contain every query term. It never exposes a Tool implementation.
func (c *Catalog) Search(ctx context.Context, policy Policy, query string) ([]Descriptor, error) {
	if c == nil {
		return nil, errors.New("catalog: nil catalog")
	}

	terms := strings.Fields(strings.ToLower(query))

	matched := make([]Descriptor, 0, len(c.entries))
	for _, entry := range c.entries {
		descriptor := describe(entry)

		allowed, err := policy.allows(ctx, descriptor)
		if err != nil {
			return nil, fmt.Errorf("catalog: authorize %q: %w", descriptor.Name, err)
		}

		if allowed && descriptorMatches(entry, terms) {
			matched = append(matched, descriptor)
		}
	}

	return matched, nil
}

// Tools selects exact tool names from an already policy-gated catalog. It is
// used by deferred tool loading after the search result has been shown.
func (c *Catalog) Tools(ctx context.Context, policy Policy, names ...string) ([]agent.Tool, error) {
	tools := make([]agent.Tool, 0, len(names))

	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			continue
		}

		seen[name] = struct{}{}

		entry, ok := c.byName[name]
		if !ok {
			return nil, fmt.Errorf("catalog: unknown tool %q", name)
		}

		allowed, err := policy.allows(ctx, describe(entry))
		if err != nil {
			return nil, fmt.Errorf("catalog: authorize %q: %w", name, err)
		}

		if !allowed {
			return nil, fmt.Errorf("catalog: tool %q is not authorized", name)
		}

		tools = append(tools, entry.Tool)
	}

	return tools, nil
}

// Local wraps application-owned tools with local provenance.
func Local(id string, risk Risk, tools ...agent.Tool) []Entry {
	return entries(Source{Kind: SourceLocal, ID: id}, risk, tools)
}

// MCP wraps a snapshot returned by agent/mcp with server provenance.
func MCP(server string, risk Risk, tools ...agent.Tool) []Entry {
	return entries(Source{Kind: SourceMCP, ID: server}, risk, tools)
}

// Extension wraps tools contributed by an installed extension.
func Extension(extensionID string, risk Risk, tools ...agent.Tool) []Entry {
	return entries(Source{Kind: SourceExtension, ID: extensionID}, risk, tools)
}

// Team wraps a snapshot returned by agent/team with team provenance.
func Team(teamID string, risk Risk, tools ...agent.Tool) []Entry {
	return entries(Source{Kind: SourceTeam, ID: teamID}, risk, tools)
}

func entries(source Source, risk Risk, tools []agent.Tool) []Entry {
	entries := make([]Entry, len(tools))
	for i, tool := range tools {
		entries[i] = Entry{Tool: tool, Source: source, Risk: risk}
	}

	return entries
}

func (p Policy) allows(ctx context.Context, descriptor Descriptor) (bool, error) {
	if descriptor.Risk > p.MaxRisk || !allowlisted(descriptor.Name, p.Allowlist) {
		return false, nil
	}

	if p.Authorize == nil {
		return true, nil
	}

	return p.Authorize(ctx, p.TenantID, descriptor)
}

func allowlisted(name string, allowlist []string) bool {
	for _, candidate := range allowlist {
		if candidate == "*" || candidate == name {
			return true
		}
	}

	return false
}

func validSource(source Source) bool {
	return (source.Kind == SourceLocal || source.Kind == SourceMCP || source.Kind == SourceExtension || source.Kind == SourceTeam) && strings.TrimSpace(source.ID) != ""
}

func describe(entry Entry) Descriptor {
	decl := entry.Tool.Decl()
	return Descriptor{Name: decl.Name, Description: decl.Description, Source: entry.Source, Risk: entry.Risk, Tags: slices.Clone(entry.Tags)}
}

func descriptorMatches(entry Entry, terms []string) bool {
	if len(terms) == 0 {
		return true
	}

	var text strings.Builder

	decl := entry.Tool.Decl()
	text.WriteString(decl.Name)
	text.WriteByte(' ')
	text.WriteString(decl.Description)
	text.WriteByte(' ')
	text.WriteString(entry.Source.Kind)
	text.WriteByte(' ')
	text.WriteString(entry.Source.ID)
	text.WriteByte(' ')
	text.WriteString(strings.Join(entry.Tags, " "))

	lower := strings.ToLower(text.String())
	for _, term := range terms {
		if !strings.Contains(lower, term) {
			return false
		}
	}

	return true
}
