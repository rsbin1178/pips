package extension

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
)

type config struct {
	capabilities Capabilities
	extensions   []Extension
}

// Option configures a Runtime.
type Option func(*config) error

// WithCapabilities adds application-specific capability names used during
// Extension negotiation.
func WithCapabilities(capabilities ...Capability) Option {
	return func(cfg *config) error {
		for _, capability := range capabilities {
			if strings.TrimSpace(string(capability)) == "" {
				return fmt.Errorf("%w: empty capability", ErrInvalid)
			}

			cfg.capabilities.values[capability] = struct{}{}
		}

		return nil
	}
}

// WithExtensions registers trusted, application-compiled Extensions. The
// registry is immutable after New returns.
func WithExtensions(extensions ...Extension) Option {
	return func(cfg *config) error {
		cfg.extensions = append(cfg.extensions, extensions...)

		return nil
	}
}

// Runtime validates and atomically publishes immutable Extension generations.
// It is safe for concurrent Acquire and activation, creates no goroutines, and
// never invokes Extension callbacks while holding its state mutex.
type Runtime struct {
	activateMu sync.Mutex
	mu         sync.Mutex

	capabilities Capabilities
	byID         map[string]Extension
	current      *generation
	next         uint64
	isClosed     bool
}

type generation struct {
	snapshot   Snapshot
	lifecycles []Lifecycle
	refs       int
	isRetired  bool
	isStopped  bool
}

// New creates an empty Runtime with built-in contribution capabilities.
func New(options ...Option) (*Runtime, error) {
	cfg := config{capabilities: builtInCapabilities()}

	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil option", ErrInvalid)
		}

		if err := option(&cfg); err != nil {
			return nil, err
		}
	}

	runtime := &Runtime{
		capabilities: cfg.capabilities.clone(),
		byID:         make(map[string]Extension, len(cfg.extensions)),
	}
	for _, ext := range cfg.extensions {
		if isNil(ext) {
			return nil, fmt.Errorf("%w: nil registered extension", ErrInvalid)
		}

		descriptor := ext.Descriptor()
		if err := validateDescriptor(descriptor); err != nil {
			return nil, err
		}

		if _, exists := runtime.byID[descriptor.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate registered extension %q", ErrInvalid, descriptor.ID)
		}

		runtime.byID[descriptor.ID] = ext
	}

	return runtime, nil
}

// Capabilities returns the immutable Runtime capability set.
func (r *Runtime) Capabilities() Capabilities {
	if r == nil {
		return Capabilities{}
	}

	return r.capabilities.clone()
}

// Resolve returns registered Extensions matching IDs in caller order. An
// empty ID list resolves to an empty result; it never implicitly selects the
// complete registry.
func (r *Runtime) Resolve(ids ...string) ([]Extension, error) {
	if r == nil {
		return nil, errors.New("extension: nil runtime")
	}

	if len(ids) == 0 {
		return nil, nil
	}

	values := make([]Extension, 0, len(ids))

	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate requested extension %q", ErrInvalid, id)
		}

		ext, ok := r.byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotRegistered, id)
		}

		seen[id] = struct{}{}

		values = append(values, ext)
	}

	return values, nil
}

// Activate prepares, validates, starts, and publishes one complete generation.
// The returned Activation is a lease and must be released. When retirement of
// an unleased prior generation fails, both the installed Activation and a
// cleanup error are returned.
func (r *Runtime) Activate(
	ctx context.Context,
	extensions ...Extension,
) (*Activation, error) {
	if r == nil {
		return nil, errors.New("extension: nil runtime")
	}

	r.activateMu.Lock()
	defer r.activateMu.Unlock()

	if err := r.closedError(); err != nil {
		return nil, err
	}

	prepared, err := prepareGeneration(ctx, r.capabilities, extensions)
	if err != nil {
		return nil, err
	}

	started, err := startLifecycles(ctx, prepared.lifecycles)
	if err != nil {
		return nil, err
	}

	prepared.lifecycles = started

	r.mu.Lock()
	r.next++
	prepared.snapshot.generation = r.next
	prepared.refs = 2 // Runtime ownership plus the returned Activation lease.

	previous := r.current
	r.current = prepared
	toStop := retireLocked(previous)
	r.mu.Unlock()

	activation := &Activation{runtime: r, generation: prepared}
	if stopErr := stopLifecycles(ctx, toStop); stopErr != nil {
		return activation, fmt.Errorf("extension: stop prior generation: %w", stopErr)
	}

	return activation, nil
}

// Acquire leases the current generation. Release the returned Activation when
// its Agent or Harness run has finished.
func (r *Runtime) Acquire() (*Activation, error) {
	if r == nil {
		return nil, errors.New("extension: nil runtime")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isClosed {
		return nil, ErrClosed
	}

	if r.current == nil {
		return nil, ErrNotActive
	}

	r.current.refs++

	return &Activation{runtime: r, generation: r.current}, nil
}

// Shutdown retires the current generation and rejects future activation. Any
// leased generation stops when its final Activation is released.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}

	r.activateMu.Lock()
	defer r.activateMu.Unlock()

	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return nil
	}

	r.isClosed = true
	current := r.current
	r.current = nil
	toStop := retireLocked(current)
	r.mu.Unlock()

	return stopLifecycles(ctx, toStop)
}

func (r *Runtime) closedError() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isClosed {
		return ErrClosed
	}

	return nil
}

// Activation is a lease on one immutable Extension generation.
type Activation struct {
	runtime    *Runtime
	generation *generation
	once       sync.Once
	releaseErr error
}

// Snapshot returns a defensive immutable view of the leased generation.
func (a *Activation) Snapshot() Snapshot {
	if a == nil || a.generation == nil {
		return Snapshot{}
	}

	return cloneSnapshot(a.generation.snapshot)
}

// Release drops this generation lease. A retired generation's lifecycles stop
// in reverse order when its final lease is released. Release is idempotent.
func (a *Activation) Release(ctx context.Context) error {
	if a == nil || a.runtime == nil || a.generation == nil {
		return nil
	}

	a.once.Do(func() {
		a.releaseErr = a.runtime.release(ctx, a.generation)
	})

	return a.releaseErr
}

func (r *Runtime) release(ctx context.Context, value *generation) error {
	r.mu.Lock()
	if value.refs <= 0 {
		r.mu.Unlock()
		return fmt.Errorf("%w: generation released too many times", ErrInvalid)
	}

	value.refs--
	toStop := stopReadyLocked(value)
	r.mu.Unlock()

	return stopLifecycles(ctx, toStop)
}

func retireLocked(value *generation) []Lifecycle {
	if value == nil {
		return nil
	}

	value.isRetired = true
	value.refs-- // Drop Runtime ownership.

	return stopReadyLocked(value)
}

func stopReadyLocked(value *generation) []Lifecycle {
	if value == nil || !value.isRetired || value.refs != 0 || value.isStopped {
		return nil
	}

	value.isStopped = true

	return slices.Clone(value.lifecycles)
}

func prepareGeneration(
	ctx context.Context,
	capabilities Capabilities,
	extensions []Extension,
) (*generation, error) {
	builder := generationBuilder{
		descriptors: make([]Descriptor, 0, len(extensions)),
		hookSets:    make([]Hooks, 0, len(extensions)),
		lifecycles:  make([]Lifecycle, 0, len(extensions)),
		seen:        make(map[string]struct{}, len(extensions)),
	}
	for _, ext := range extensions {
		if err := builder.add(ctx, capabilities, ext); err != nil {
			return nil, err
		}
	}

	return builder.finish()
}

type generationBuilder struct {
	descriptors []Descriptor
	diagnostics []Diagnostic
	toolEntries []catalog.Entry
	skills      []SkillEntry
	prompts     []PromptEntry
	assets      []AssetEntry
	middleware  []ai.Middleware
	hookSets    []Hooks
	lifecycles  []Lifecycle
	seen        map[string]struct{}
}

func (b *generationBuilder) add(
	ctx context.Context,
	capabilities Capabilities,
	ext Extension,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if isNil(ext) {
		return fmt.Errorf("%w: nil extension", ErrInvalid)
	}

	descriptor := ext.Descriptor()
	if err := validateDescriptor(descriptor); err != nil {
		return err
	}

	if _, duplicate := b.seen[descriptor.ID]; duplicate {
		return fmt.Errorf("%w: duplicate active extension %q", ErrInvalid, descriptor.ID)
	}

	b.seen[descriptor.ID] = struct{}{}

	missing, optional := negotiate(capabilities, descriptor)
	if len(missing) > 0 {
		return fmt.Errorf(
			"%w: extension %q requires %q",
			ErrCapabilityUnavailable,
			descriptor.ID,
			missing[0],
		)
	}

	origin := Origin{ExtensionID: descriptor.ID, Version: descriptor.Version}
	for _, capability := range optional {
		b.diagnostics = append(b.diagnostics, Diagnostic{
			Severity:   SeverityWarning,
			Origin:     origin,
			Capability: capability,
			Message:    "optional capability is unavailable",
		})
	}

	contribution, err := ext.Prepare(ctx)
	if err != nil {
		return fmt.Errorf("extension: prepare %q: %w", descriptor.ID, err)
	}

	if err := validateContribution(descriptor, contribution); err != nil {
		return err
	}

	b.addContribution(descriptor, origin, contribution)

	return nil
}

func (b *generationBuilder) addContribution(
	descriptor Descriptor,
	origin Origin,
	contribution Contribution,
) {
	for _, tool := range contribution.Tools {
		b.toolEntries = append(b.toolEntries, catalog.Entry{
			Tool: tool.Value,
			Source: catalog.Source{
				Kind: catalog.SourceExtension,
				ID:   descriptor.ID,
			},
			Risk: tool.Risk,
			Tags: slices.Clone(tool.Tags),
		})
	}

	for _, skill := range contribution.Skills {
		b.skills = append(b.skills, SkillEntry{Origin: origin, Skill: cloneSkill(skill)})
	}

	for _, prompt := range contribution.Prompts {
		b.prompts = append(b.prompts, PromptEntry{Origin: origin, Template: prompt})
	}

	for _, asset := range contribution.Assets {
		b.assets = append(b.assets, AssetEntry{Origin: origin, Asset: cloneAsset(asset)})
	}

	b.middleware = append(b.middleware, contribution.Middleware...)
	b.descriptors = append(b.descriptors, cloneDescriptor(descriptor))

	b.hookSets = append(b.hookSets, contribution.Hooks)
	if contribution.Lifecycle != nil {
		b.lifecycles = append(b.lifecycles, contribution.Lifecycle)
	}
}

func (b *generationBuilder) finish() (*generation, error) {
	catalogValue, err := catalog.New(b.toolEntries...)
	if err != nil {
		return nil, fmt.Errorf("%w: validate tools: %w", ErrInvalid, err)
	}

	skillValues := make([]harness.Skill, len(b.skills))
	for i, entry := range b.skills {
		skillValues[i] = entry.Skill
	}

	if _, err := harness.NewSkillCatalog(skillValues...); err != nil {
		return nil, fmt.Errorf("%w: validate skills: %w", ErrInvalid, err)
	}

	promptValues := make([]harness.PromptTemplate, len(b.prompts))
	for i, entry := range b.prompts {
		promptValues[i] = entry.Template
	}

	if err := harness.ValidateTemplates(promptValues...); err != nil {
		return nil, fmt.Errorf("%w: validate prompts: %w", ErrInvalid, err)
	}

	if err := validateAssetEntries(b.assets); err != nil {
		return nil, err
	}

	return &generation{
		snapshot: Snapshot{
			descriptors: b.descriptors,
			diagnostics: b.diagnostics,
			catalog:     catalogValue,
			skills:      b.skills,
			prompts:     b.prompts,
			assets:      b.assets,
			middleware:  b.middleware,
			hooks:       ComposeHooks(b.hookSets...),
		},
		lifecycles: b.lifecycles,
	}, nil
}

func validateDescriptor(descriptor Descriptor) error {
	if strings.TrimSpace(descriptor.ID) == "" || len(descriptor.ID) > 256 {
		return fmt.Errorf("%w: extension id is empty or too long", ErrInvalid)
	}

	if strings.TrimSpace(descriptor.Version) == "" || len(descriptor.Version) > 128 {
		return fmt.Errorf("%w: extension %q version is empty or too long", ErrInvalid, descriptor.ID)
	}

	if strings.IndexFunc(descriptor.ID, func(r rune) bool { return r <= ' ' }) >= 0 {
		return fmt.Errorf("%w: extension id %q contains whitespace", ErrInvalid, descriptor.ID)
	}

	seen := make(map[Capability]struct{}, len(descriptor.Requires)+len(descriptor.Optional))
	for _, capability := range append(slices.Clone(descriptor.Requires), descriptor.Optional...) {
		if strings.TrimSpace(string(capability)) == "" {
			return fmt.Errorf("%w: extension %q has an empty capability", ErrInvalid, descriptor.ID)
		}

		if _, duplicate := seen[capability]; duplicate {
			return fmt.Errorf("%w: extension %q repeats capability %q", ErrInvalid, descriptor.ID, capability)
		}

		seen[capability] = struct{}{}
	}

	return nil
}

func validateContribution(descriptor Descriptor, contribution Contribution) error {
	for _, tool := range contribution.Tools {
		if isNil(tool.Value) {
			return fmt.Errorf("%w: extension %q contributed a nil tool", ErrInvalid, descriptor.ID)
		}

		if tool.Risk > catalog.RiskPrivileged {
			return fmt.Errorf("%w: extension %q contributed an invalid tool risk", ErrInvalid, descriptor.ID)
		}
	}

	for _, middleware := range contribution.Middleware {
		if middleware == nil {
			return fmt.Errorf("%w: extension %q contributed nil middleware", ErrInvalid, descriptor.ID)
		}
	}

	if contribution.Lifecycle != nil && isNil(contribution.Lifecycle) {
		return fmt.Errorf("%w: extension %q contributed a nil lifecycle", ErrInvalid, descriptor.ID)
	}

	return nil
}

func validateAssetEntries(entries []AssetEntry) error {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		kind := strings.TrimSpace(entry.Asset.Kind)

		name := strings.TrimSpace(entry.Asset.Name)
		if kind == "" || name == "" {
			return fmt.Errorf("%w: extension %q contributed an asset without kind or name", ErrInvalid, entry.Origin.ExtensionID)
		}

		key := kind + "\x00" + name
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: duplicate asset %q/%q", ErrInvalid, kind, name)
		}

		seen[key] = struct{}{}
	}

	return nil
}

func negotiate(capabilities Capabilities, descriptor Descriptor) (required, optional []Capability) {
	for _, capability := range descriptor.Requires {
		if !capabilities.Has(capability) {
			required = append(required, capability)
		}
	}

	for _, capability := range descriptor.Optional {
		if !capabilities.Has(capability) {
			optional = append(optional, capability)
		}
	}

	return required, optional
}

func startLifecycles(ctx context.Context, lifecycles []Lifecycle) ([]Lifecycle, error) {
	started := make([]Lifecycle, 0, len(lifecycles))
	for _, lifecycle := range lifecycles {
		if err := lifecycle.Start(ctx); err != nil {
			rollbackErr := stopLifecycles(ctx, started)
			return nil, errors.Join(fmt.Errorf("extension: start lifecycle: %w", err), rollbackErr)
		}

		started = append(started, lifecycle)
	}

	return started, nil
}

func stopLifecycles(ctx context.Context, lifecycles []Lifecycle) error {
	errs := make([]error, 0)

	for _, lifecycle := range slices.Backward(lifecycles) {
		if err := lifecycle.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func isNil(value any) bool {
	if value == nil {
		return true
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
