package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rsbin/pips/agent"
)

const toolSearchName = "tool_search"

// ToolSearchOptions controls deferred loading behavior. Initial tools are
// application built-ins and remain visible on every snapshot. When Enabled is
// false, every policy-authorized catalog tool is exposed normally. When true,
// only DeferredSources are discovered through tool_search; all other sources
// stay visible. An empty DeferredSources uses the Claude Code-like default:
// MCP and extension tools are deferred, while local and Team tools are direct.
type ToolSearchOptions struct {
	Enabled         bool
	DeferredSources []string
	Initial         []agent.Tool
	Limit           int
	MaxRuns         int
}

// ToolSearch provides Claude-style deferred tool discovery without exposing
// every declaration on the first model request. Add Tools() when building the
// agent and use PrepareTurn as its prepare-turn hook. Search results are
// policy-filtered before they reach the model, then re-authorized when the
// snapshot is applied.
type ToolSearch struct {
	catalog  *Catalog
	policy   Policy
	initial  []agent.Tool
	deferred map[string]struct{}
	enabled  bool
	limit    int
	maxRuns  int
	tool     agent.Tool

	mu   sync.Mutex
	runs map[string]searchRun
}

type searchRun struct {
	names  []string
	usedAt time.Time
}

// NewToolSearch constructs a source-aware tool configuration. policy must
// explicitly authorize every eventual target; an empty allowlist produces an
// empty tool snapshot. Call Tools with the construction context, then install
// PrepareTurn only when Enabled is true.
func NewToolSearch(catalog *Catalog, policy Policy, options ToolSearchOptions) (*ToolSearch, error) {
	if catalog == nil {
		return nil, errors.New("catalog: tool search requires a catalog")
	}

	if options.Limit <= 0 {
		options.Limit = 8
	}

	if options.MaxRuns <= 0 {
		options.MaxRuns = 64
	}

	if len(options.DeferredSources) == 0 {
		options.DeferredSources = []string{SourceMCP, SourceExtension}
	}

	if err := uniqueToolNames(append(slices.Clone(options.Initial), agent.NewTool(toolSearchName, "", func(context.Context, struct{}) (string, error) { return "", nil }))); err != nil {
		return nil, err
	}

	if _, exists := catalog.byName[toolSearchName]; exists {
		return nil, fmt.Errorf("catalog: %q is reserved for deferred tool discovery", toolSearchName)
	}

	for _, tool := range options.Initial {
		if _, exists := catalog.byName[tool.Decl().Name]; exists {
			return nil, fmt.Errorf("catalog: initial tool %q duplicates a searchable tool", tool.Decl().Name)
		}
	}

	s := &ToolSearch{
		catalog:  catalog,
		policy:   policy,
		initial:  slices.Clone(options.Initial),
		deferred: sourceSet(options.DeferredSources),
		enabled:  options.Enabled,
		limit:    options.Limit,
		maxRuns:  options.MaxRuns,
		runs:     make(map[string]searchRun),
	}
	s.tool = agent.NewTool(toolSearchName, "Search and activate policy-authorized tools for the next turn.", s.search)

	return s, nil
}

type searchArgs struct {
	Query string   `json:"query" description:"Capability to search for."`
	Tools []string `json:"tools,omitempty" description:"Exact result names to activate. If omitted, the best matching results are activated."`
}

type searchResult struct {
	Matches   []Descriptor `json:"matches"`
	Activated []string     `json:"activated"`
}

// Tools returns the application built-ins and all direct catalog tools. When
// deferred search is enabled, it additionally returns tool_search; otherwise
// it returns every policy-authorized catalog tool. The slice is suitable for
// agent.WithTools.
func (s *ToolSearch) Tools(ctx context.Context) ([]agent.Tool, error) {
	if s == nil {
		return nil, nil
	}

	catalogTools, err := s.catalog.Snapshot(ctx, s.policy)
	if err != nil {
		return nil, err
	}

	if s.enabled {
		catalogTools = s.direct(catalogTools)
	}

	tools := make([]agent.Tool, 0, len(s.initial)+len(catalogTools)+1)
	tools = append(tools, s.initial...)

	tools = append(tools, catalogTools...)
	if s.enabled {
		tools = append(tools, s.tool)
	}

	return tools, nil
}

// AgentOptions builds the options needed to install this configuration. It
// keeps direct tools visible and adds the prepare hook only when search is on.
func (s *ToolSearch) AgentOptions(ctx context.Context) ([]agent.Option, error) {
	tools, err := s.Tools(ctx)
	if err != nil {
		return nil, err
	}

	opts := []agent.Option{agent.WithTools(tools...)}
	if s.enabled {
		opts = append(opts, agent.WithPrepareTurn(s.PrepareTurn))
	}

	return opts, nil
}

// PrepareTurn returns the complete next-turn snapshot. It must be installed
// through agent.WithPrepareTurn; the agent runtime validates the replacement
// before showing it to the model.
func (s *ToolSearch) PrepareTurn(ctx context.Context, info agent.RunInfo) agent.TurnUpdate {
	if s == nil || !s.enabled {
		return agent.TurnUpdate{}
	}

	tools, err := s.snapshot(ctx, info.RunID)
	if err != nil {
		// Keep the previous snapshot on an authorization failure. The search
		// tool itself returns errors to the model at the triggering turn.
		return agent.TurnUpdate{}
	}

	return agent.TurnUpdate{Tools: tools}
}

// Forget removes one run's deferred selection early. It is useful when a
// host chains its event handler and observes agent.EventRunCompleted.
func (s *ToolSearch) Forget(runID string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	delete(s.runs, runID)
	s.mu.Unlock()
}

//nolint:gocyclo // The checks are the security boundary for deferred activation.
func (s *ToolSearch) search(ctx context.Context, args searchArgs) (string, error) {
	meta, ok := agent.RunMetadataFromContext(ctx)
	if !ok || meta.RunID == "" {
		return "", errors.New("tool_search must run inside an agent invocation")
	}

	query := strings.TrimSpace(args.Query)
	if query == "" && len(args.Tools) == 0 {
		return "", errors.New("query or tools is required")
	}

	matches, err := s.catalog.Search(ctx, s.policy, query)
	if err != nil {
		return "", err
	}

	matches = s.deferredMatches(matches)
	if len(matches) > s.limit {
		matches = matches[:s.limit]
	}

	available := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		available[match.Name] = struct{}{}
	}

	selected := slices.Clone(args.Tools)
	if len(selected) == 0 {
		for _, match := range matches {
			selected = append(selected, match.Name)
		}
	}

	selected = uniqueStrings(selected)
	for _, name := range selected {
		if _, ok := available[name]; !ok {
			return "", fmt.Errorf("tool %q was not returned by this search", name)
		}
	}

	if _, err := s.catalog.Tools(ctx, s.policy, selected...); err != nil {
		return "", err
	}

	s.mu.Lock()
	s.pruneLocked()
	s.runs[meta.RunID] = searchRun{names: selected, usedAt: time.Now().UTC()}
	s.mu.Unlock()

	result, err := json.Marshal(searchResult{Matches: matches, Activated: selected})
	if err != nil {
		return "", fmt.Errorf("encode tool search result: %w", err)
	}

	return string(result), nil
}

func (s *ToolSearch) snapshot(ctx context.Context, runID string) ([]agent.Tool, error) {
	s.mu.Lock()

	state := s.runs[runID]
	if state.names != nil {
		state.usedAt = time.Now().UTC()
		s.runs[runID] = state
	}
	s.mu.Unlock()

	selected, err := s.catalog.Tools(ctx, s.policy, state.names...)
	if err != nil {
		return nil, err
	}

	tools, err := s.Tools(ctx)
	if err != nil {
		return nil, err
	}

	tools = append(tools, selected...)

	return tools, nil
}

func (s *ToolSearch) direct(tools []agent.Tool) []agent.Tool {
	direct := make([]agent.Tool, 0, len(tools))
	for _, tool := range tools {
		entry := s.catalog.byName[tool.Decl().Name]
		if _, deferred := s.deferred[entry.Source.Kind]; !deferred {
			direct = append(direct, tool)
		}
	}

	return direct
}

func (s *ToolSearch) deferredMatches(matches []Descriptor) []Descriptor {
	filtered := make([]Descriptor, 0, len(matches))
	for _, match := range matches {
		if _, deferred := s.deferred[match.Source.Kind]; deferred {
			filtered = append(filtered, match)
		}
	}

	return filtered
}

func sourceSet(sources []string) map[string]struct{} {
	set := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		set[source] = struct{}{}
	}

	return set
}

func (s *ToolSearch) pruneLocked() {
	for len(s.runs) >= s.maxRuns {
		var (
			oldest   string
			oldestAt time.Time
		)
		for runID, state := range s.runs {
			if oldest == "" || state.usedAt.Before(oldestAt) {
				oldest, oldestAt = runID, state.usedAt
			}
		}

		delete(s.runs, oldest)
	}
}

func uniqueToolNames(tools []agent.Tool) error {
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if tool == nil {
			return errors.New("catalog: tool search received nil initial tool")
		}

		name := tool.Decl().Name
		if name == "" {
			return errors.New("catalog: tool search received an unnamed initial tool")
		}

		if _, exists := seen[name]; exists {
			return fmt.Errorf("catalog: tool search duplicate initial tool %q", name)
		}

		seen[name] = struct{}{}
	}

	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))

	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}

		seen[value] = struct{}{}
		out = append(out, value)
	}

	return out
}
