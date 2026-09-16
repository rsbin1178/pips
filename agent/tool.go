package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/rsbin1178/pips/ai"
)

// Tool is a capability the model may invoke during a run. Decl describes the
// tool to the model; Exec performs the call. Implementations must be safe for
// concurrent use when they also implement [ConcurrencySafe].
//
// Exec errors do not abort the run: the runtime converts them (along with
// panics, timeouts, and undecodable arguments) into error tool results that
// are fed back to the model, which typically corrects course. Most tools are
// easier to write with [NewTool] than by implementing the interface directly.
type Tool interface {
	// Decl returns the declaration advertised to the model.
	Decl() ai.Tool
	// Exec runs the call and returns the result content. A nil part slice
	// with a nil error is a valid empty result.
	Exec(ctx context.Context, call ToolCall) ([]ai.Part, error)
}

// ToolCall is the invocation passed to [Tool.Exec].
type ToolCall struct {
	// ID is the provider-assigned call identifier.
	ID string
	// Name is the tool being called.
	Name string
	// Args is the raw JSON arguments object produced by the model.
	Args ai.JSON
}

// ConcurrencySafe is an optional interface a [Tool] may implement to declare
// that it can run in parallel with other concurrency-safe tools of the same
// turn. Tools that do not implement it (or report false) run serially, each
// acting as a barrier within the turn's batch. Wrap an existing tool with
// [Parallel] instead of implementing this by hand.
type ConcurrencySafe interface {
	// Concurrent reports whether the tool may execute in parallel.
	Concurrent() bool
}

// Parallel marks a tool as safe for parallel execution within a turn (see
// [ConcurrencySafe]). Only mark tools whose Exec is free of shared mutable
// state, such as read-only lookups.
func Parallel(t Tool) Tool {
	return parallelTool{t}
}

type parallelTool struct {
	Tool
}

func (parallelTool) Concurrent() bool { return true }

// TextResult wraps s as single-part tool result content. It is a convenience
// for hand-written [Tool] implementations; [NewTool] applies it automatically.
func TextResult(s string) []ai.Part {
	return []ai.Part{ai.TextPart{Text: s}}
}

// NewTool defines a tool from a typed Go function. The argument schema is
// derived from Args with [ai.SchemaFor] (json tags name fields, description
// tags document them), and the model's JSON arguments are decoded into Args
// before fn runs. Use struct{} for a tool that takes no arguments.
//
// NewTool panics when a schema cannot be derived from Args — like
// [regexp.MustCompile], it is meant for declarations whose type is fixed at
// compile time. Implement [Tool] directly for dynamic schemas.
func NewTool[Args any](name, description string, fn func(ctx context.Context, args Args) (string, error)) Tool {
	return NewToolParts(name, description, func(ctx context.Context, args Args) ([]ai.Part, error) {
		text, err := fn(ctx, args)
		if err != nil && !errors.Is(err, ErrTerminate) {
			return nil, err
		}

		// ErrTerminate is a control sentinel: the text is a real result that
		// must survive alongside it.
		return TextResult(text), err
	})
}

// NewToolParts is [NewTool] for tools whose results are multi-modal (for
// example an image-producing tool) rather than plain text.
func NewToolParts[Args any](name, description string, fn func(ctx context.Context, args Args) ([]ai.Part, error)) Tool {
	decl := ai.Tool{Name: name, Description: description}
	noArgs := isEmptyStruct[Args]()

	if !noArgs {
		schema, err := ai.SchemaFor[Args]()
		if err != nil {
			panic(fmt.Sprintf("agent: NewTool %q: %v", name, err))
		}

		decl.InputSchema = schema
	}

	return &funcTool[Args]{decl: decl, fn: fn, noArgs: noArgs}
}

// isEmptyStruct reports whether Args is struct{} (or an equivalent fieldless
// struct), which declares a no-argument tool.
func isEmptyStruct[Args any]() bool {
	t := reflect.TypeFor[Args]()
	return t.Kind() == reflect.Struct && t.NumField() == 0
}

type funcTool[Args any] struct {
	decl   ai.Tool
	fn     func(ctx context.Context, args Args) ([]ai.Part, error)
	noArgs bool
}

func (t *funcTool[Args]) Decl() ai.Tool {
	return t.decl
}

func (t *funcTool[Args]) Exec(ctx context.Context, call ToolCall) ([]ai.Part, error) {
	var args Args

	if !t.noArgs && len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	return t.fn(ctx, args)
}

// toolbox indexes an agent's tools by name and keeps their declarations in
// registration order for stable requests.
type toolbox struct {
	byName map[string]Tool
	decls  []ai.Tool
}

func newToolbox(tools []Tool) (*toolbox, error) {
	box := &toolbox{byName: make(map[string]Tool, len(tools))}

	for _, t := range tools {
		decl := t.Decl()
		if decl.Name == "" {
			return nil, fmt.Errorf("agent: tool with empty name (%T)", t)
		}

		if _, exists := box.byName[decl.Name]; exists {
			return nil, fmt.Errorf("agent: duplicate tool name %q", decl.Name)
		}

		box.byName[decl.Name] = t
		box.decls = append(box.decls, decl)
	}

	return box, nil
}

// concurrent reports whether the named tool declared itself safe for
// parallel execution.
func (b *toolbox) concurrent(name string) bool {
	t, ok := b.byName[name]
	if !ok {
		return false
	}

	cs, ok := t.(ConcurrencySafe)

	return ok && cs.Concurrent()
}

// ProviderTool wraps a provider-executed ai.Tool (such as Gemini's GoogleSearch
// or OpenAI's WebSearch) as an agent.Tool. Provider-executed tools are managed
// server-side by the model provider, so their Exec method is a no-op.
func ProviderTool(tool ai.Tool) Tool {
	return &providerTool{tool: tool}
}

type providerTool struct {
	tool ai.Tool
}

func (p *providerTool) Decl() ai.Tool {
	return p.tool
}

func (p *providerTool) Exec(ctx context.Context, call ToolCall) ([]ai.Part, error) {
	return nil, nil
}
