package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

// DefaultToolSearchName is the namespaced discovery tool name used when
// ToolSearchOptions.Name is empty. The pips_ prefix avoids collisions with
// proxies and providers that reserve or rewrite the bare "tool_search" name.
const DefaultToolSearchName = "pips_tool_search"

var toolSearchNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidateToolSearchName reports whether name is portable across providers:
// one to sixty-four ASCII letters, digits, underscores, or dashes.
func ValidateToolSearchName(name string) error {
	if !toolSearchNamePattern.MatchString(name) {
		return fmt.Errorf("catalog: tool search name %q must match %s", name, toolSearchNamePattern)
	}

	return nil
}

// ToolSearchOptions controls deferred loading behavior. Initial tools are
// application built-ins and remain visible on every snapshot. When Enabled is
// false, every policy-authorized catalog tool is exposed normally. When true,
// only DeferredSources are discovered through the discovery tool; all other
// sources stay visible. An empty DeferredSources uses the Claude Code-like
// default: MCP and extension tools are deferred, while local and Team tools
// are direct. Name overrides DefaultToolSearchName; it must satisfy
// ValidateToolSearchName and must not collide with a catalog or Initial tool.
type ToolSearchOptions struct {
	Enabled         bool
	Name            string
	DeferredSources []string
	Initial         []agent.Tool
	Limit           int
	MaxRuns         int
}

// SourceSummary counts the policy-authorized deferred tools registered under
// one source. It is reported to the model so a miss can be distinguished from
// an empty registry.
type SourceSummary struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Tools int    `json:"tools"`
}

// String renders the summary as kind/id (count).
func (s SourceSummary) String() string {
	return fmt.Sprintf("%s/%s (%d)", s.Kind, s.ID, s.Tools)
}

// FormatSourceSummaries joins summaries for a prompt or hint sentence.
func FormatSourceSummaries(sources []SourceSummary) string {
	parts := make([]string, len(sources))
	for index, source := range sources {
		parts[index] = source.String()
	}

	return strings.Join(parts, ", ")
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
	name     string
	limit    int
	maxRuns  int
	tool     agent.Tool
	// hidden is the same discovery handler with a Disabled declaration. It
	// keeps a blind call by name executable when no deferred tool exists, so
	// the model receives the registry hint instead of an unknown-tool error.
	hidden agent.Tool

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

	if options.Name == "" {
		options.Name = DefaultToolSearchName
	}

	if err := ValidateToolSearchName(options.Name); err != nil {
		return nil, err
	}

	if err := uniqueToolNames(append(slices.Clone(options.Initial), agent.NewTool(options.Name, "", func(context.Context, struct{}) (string, error) { return "", nil }))); err != nil {
		return nil, err
	}

	if _, exists := catalog.byName[options.Name]; exists {
		return nil, fmt.Errorf("catalog: %q is reserved for deferred tool discovery", options.Name)
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
		name:     options.Name,
		limit:    options.Limit,
		maxRuns:  options.MaxRuns,
		runs:     make(map[string]searchRun),
	}
	s.tool = agent.NewTool(
		options.Name,
		"Search the deferred MCP and Extension tools by capability and activate the best matches for the next turn. Built-in tools are already visible; do not search for them.",
		s.search,
	)
	s.hidden = hiddenTool{tool: s.tool}

	return s, nil
}

type hiddenTool struct {
	tool agent.Tool
}

func (h hiddenTool) Decl() ai.Tool {
	decl := h.tool.Decl()
	decl.Disabled = true

	return decl
}

func (h hiddenTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	return h.tool.Exec(ctx, call)
}

// Name returns the discovery tool name the model must call.
func (s *ToolSearch) Name() string {
	if s == nil {
		return ""
	}

	return s.name
}

type searchArgs struct {
	Query string   `json:"query" description:"Capability to search for."`
	Tools []string `json:"tools,omitempty" description:"Exact result names to activate. If omitted, the best matching results are activated."`
}

type searchResult struct {
	Matches   []Descriptor    `json:"matches"`
	Activated []string        `json:"activated"`
	Sources   []SourceSummary `json:"sources"`
	Hint      string          `json:"hint,omitempty"`
}

const (
	hintNothingDeferred = "No MCP or Extension tools are registered in this session; nothing can be discovered."
	hintNoMatch         = "No deferred tool matched. Searchable sources: %s. Try broader or different terms."
)

// Tools returns the model-visible toolset: the application built-ins and all
// direct catalog tools. When deferred search is enabled and at least one
// authorized deferred tool exists, it additionally returns the discovery
// tool; when search is disabled it returns every policy-authorized catalog
// tool. Use it for prompts and visibility decisions.
func (s *ToolSearch) Tools(ctx context.Context) ([]agent.Tool, error) {
	tools, _, err := s.tools(ctx)

	return tools, err
}

// ExecutableTools returns Tools plus, when search is enabled but nothing is
// deferred, a hidden copy of the discovery tool. The copy is omitted from
// model requests yet executes when called by name, answering with the
// registry hint. Pass this slice to agent.WithTools.
func (s *ToolSearch) ExecutableTools(ctx context.Context) ([]agent.Tool, error) {
	tools, discoverable, err := s.tools(ctx)
	if err != nil || s == nil || !s.enabled || discoverable {
		return tools, err
	}

	return append(tools, s.hidden), nil
}

func (s *ToolSearch) tools(ctx context.Context) ([]agent.Tool, bool, error) {
	if s == nil {
		return nil, false, nil
	}

	catalogTools, err := s.catalog.Snapshot(ctx, s.policy)
	if err != nil {
		return nil, false, err
	}

	discoverable := false
	if s.enabled {
		catalogTools, discoverable = s.direct(catalogTools)
	}

	tools := make([]agent.Tool, 0, len(s.initial)+len(catalogTools)+1)
	tools = append(tools, s.initial...)

	tools = append(tools, catalogTools...)
	if discoverable {
		tools = append(tools, s.tool)
	}

	return tools, discoverable, nil
}

// DeferredSources summarizes the policy-authorized deferred tools grouped by
// source in registration order. It is empty when nothing can be discovered,
// whether because search is disabled or no deferred tool is registered.
func (s *ToolSearch) DeferredSources(ctx context.Context) ([]SourceSummary, error) {
	if s == nil || !s.enabled {
		return nil, nil
	}

	descriptors, err := s.catalog.Search(ctx, s.policy, "")
	if err != nil {
		return nil, err
	}

	return s.summarize(descriptors), nil
}

// AgentOptions builds the options needed to install this configuration. It
// keeps direct tools visible and adds the prepare hook only when search is on.
func (s *ToolSearch) AgentOptions(ctx context.Context) ([]agent.Option, error) {
	tools, err := s.ExecutableTools(ctx)
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
		return "", fmt.Errorf("%s must run inside an agent invocation", s.name)
	}

	sources, err := s.DeferredSources(ctx)
	if err != nil {
		return "", err
	}

	// An empty registry is an answer, not an argument error: the model must
	// learn that nothing is discoverable instead of retrying other phrasings.
	if len(sources) == 0 {
		return encodeSearchResult(searchResult{
			Matches: []Descriptor{}, Activated: []string{}, Sources: []SourceSummary{},
			Hint: hintNothingDeferred,
		})
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

	hint := ""
	if len(matches) == 0 {
		hint = fmt.Sprintf(hintNoMatch, FormatSourceSummaries(sources))
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

	return encodeSearchResult(searchResult{
		Matches: matches, Activated: selected, Sources: sources, Hint: hint,
	})
}

func encodeSearchResult(result searchResult) (string, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode tool search result: %w", err)
	}

	return string(encoded), nil
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

	tools, err := s.ExecutableTools(ctx)
	if err != nil {
		return nil, err
	}

	tools = append(tools, selected...)

	return tools, nil
}

// direct splits an authorized snapshot into the directly visible tools and
// reports whether any authorized tool was deferred instead.
func (s *ToolSearch) direct(tools []agent.Tool) ([]agent.Tool, bool) {
	direct := make([]agent.Tool, 0, len(tools))
	discoverable := false
	for _, tool := range tools {
		entry := s.catalog.byName[tool.Decl().Name]
		if _, deferred := s.deferred[entry.Source.Kind]; deferred {
			discoverable = true
			continue
		}

		direct = append(direct, tool)
	}

	return direct, discoverable
}

func (s *ToolSearch) summarize(descriptors []Descriptor) []SourceSummary {
	summaries := make([]SourceSummary, 0)
	index := make(map[Source]int)

	for _, descriptor := range descriptors {
		if _, deferred := s.deferred[descriptor.Source.Kind]; !deferred {
			continue
		}

		position, ok := index[descriptor.Source]
		if !ok {
			position = len(summaries)
			index[descriptor.Source] = position
			summaries = append(summaries, SourceSummary{Kind: descriptor.Source.Kind, ID: descriptor.Source.ID})
		}

		summaries[position].Tools++
	}

	return summaries
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
