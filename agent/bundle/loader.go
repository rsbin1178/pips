package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent/extension"
	"github.com/rsbin1178/pips/agent/harness"
)

type loaderConfig struct {
	limits Limits
}

// Option configures a Loader.
type Option func(*loaderConfig) error

// WithLimits replaces all Loader bounds. Every field must be positive.
func WithLimits(limits Limits) Option {
	return func(cfg *loaderConfig) error {
		if err := validateLimits(limits); err != nil {
			return err
		}

		cfg.limits = limits

		return nil
	}
}

// Loader decodes Bundles from one filesystem boundary. It never opens
// network connections or executes files found in that filesystem.
type Loader struct {
	mu       sync.RWMutex
	fsys     fs.FS
	limits   Limits
	close    func() error
	closed   bool
	closeErr error
}

// New creates a Loader over fsys. The caller retains ownership of fsys.
func New(fsys fs.FS, options ...Option) (*Loader, error) {
	if fsys == nil {
		return nil, fmt.Errorf("%w: nil filesystem", ErrInvalid)
	}

	cfg := loaderConfig{limits: DefaultLimits()}

	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil option", ErrInvalid)
		}

		if err := option(&cfg); err != nil {
			return nil, err
		}
	}

	return &Loader{fsys: fsys, limits: cfg.limits}, nil
}

// Open creates a Loader rooted at a local directory. The os.Root-backed
// filesystem prevents symlinks from escaping root. Close the Loader when it
// is no longer needed.
func Open(root string, options ...Option) (*Loader, error) {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("bundle: open root: %w", err)
	}

	loader, err := New(rootFS.FS(), options...)
	if err != nil {
		_ = rootFS.Close()
		return nil, err
	}

	loader.close = rootFS.Close

	return loader, nil
}

// Close releases a disk root opened by Open. It does not close filesystems
// supplied to New and is idempotent.
func (l *Loader) Close() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return l.closeErr
	}

	l.closed = true
	if l.close != nil {
		l.closeErr = l.close()
	}

	return l.closeErr
}

// Load decodes one manifest and all filtered declarative resources. Resource
// paths are relative to the manifest's directory.
func (l *Loader) Load(
	ctx context.Context,
	manifestPath string,
	settings Settings,
) (*Bundle, error) {
	if l == nil {
		return nil, fmt.Errorf("%w: nil loader", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := validateLocalPath(manifestPath); err != nil {
		return nil, fmt.Errorf("bundle: manifest path: %w", err)
	}

	if err := validateSettings(settings); err != nil {
		return nil, err
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.closed {
		return nil, ErrClosed
	}

	manifest, err := l.loadManifest(manifestPath)
	if err != nil {
		return nil, err
	}

	selected, err := filterManifest(manifest, settings.Filters)
	if err != nil {
		return nil, err
	}

	diagnostics := filterDiagnostics(manifest, selected)

	bundleFS, err := subFS(l.fsys, path.Dir(manifestPath))
	if err != nil {
		return nil, fmt.Errorf("bundle: open manifest directory: %w", err)
	}

	resources, err := l.loadResources(ctx, bundleFS, selected)
	if err != nil {
		return nil, err
	}

	return &Bundle{
		manifest:     cloneManifest(manifest),
		scope:        settings.Scope,
		extensionIDs: slices.Clone(selected.Extensions),
		skills:       resources.skills,
		prompts:      resources.prompts,
		assets:       resources.assets,
		diagnostics:  diagnostics,
	}, nil
}

func (l *Loader) loadManifest(manifestPath string) (Manifest, error) {
	data, err := readFileLimit(l.fsys, manifestPath, l.limits.MaxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("bundle: read manifest %q: %w", manifestPath, err)
	}

	manifest, err := decodeManifest(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: decode manifest %q: %w", ErrInvalid, manifestPath, err)
	}

	if err := validateManifest(manifest, l.limits); err != nil {
		return Manifest{}, err
	}

	return manifest, nil
}

type loadedResources struct {
	skills  []harness.Skill
	prompts []harness.PromptTemplate
	assets  []extension.Asset
}

func (l *Loader) loadResources(
	ctx context.Context,
	fsys fs.FS,
	manifest Manifest,
) (loadedResources, error) {
	limited := &boundedFS{
		fsys:    fsys,
		budget:  &readBudget{maxTotal: l.limits.MaxTotalBytes},
		maxFile: l.limits.MaxResourceBytes,
	}
	counter := &resourceCounter{max: l.limits.MaxResources}

	skills, err := loadSkills(ctx, limited, manifest.Skills, counter)
	if err != nil {
		return loadedResources{}, err
	}

	prompts, err := loadPrompts(ctx, limited, manifest.Prompts, counter)
	if err != nil {
		return loadedResources{}, err
	}

	assets, err := loadAssets(ctx, limited, manifest.Assets, counter)
	if err != nil {
		return loadedResources{}, err
	}

	if err := validateLoadedResources(skills, prompts); err != nil {
		return loadedResources{}, err
	}

	return loadedResources{skills: skills, prompts: prompts, assets: assets}, nil
}

func validateLimits(limits Limits) error {
	if limits.MaxManifestBytes <= 0 || limits.MaxResourceBytes <= 0 || limits.MaxTotalBytes <= 0 || limits.MaxResources <= 0 {
		return fmt.Errorf("%w: every limit must be positive", ErrInvalid)
	}

	return nil
}

func validateSettings(settings Settings) error {
	switch settings.Scope {
	case ScopeManaged, ScopeUser, ScopeProject, ScopeTemporary:
	default:
		return fmt.Errorf("%w: unsupported scope %q", ErrInvalid, settings.Scope)
	}

	switch settings.Trust {
	case TrustUnspecified, TrustDenied, TrustApproved:
	default:
		return fmt.Errorf("%w: unsupported trust decision %d", ErrInvalid, settings.Trust)
	}

	if settings.Trust == TrustDenied {
		return ErrUntrusted
	}

	if settings.Scope == ScopeProject && settings.Trust != TrustApproved {
		return fmt.Errorf("%w: project bundles require explicit approval", ErrUntrusted)
	}

	return nil
}

func decodeManifest(data []byte) (Manifest, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return Manifest{}, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, err
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("multiple JSON values")
		}

		return Manifest{}, err
	}

	return manifest, nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}

	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delimiter {
	case '{':
		seen := make(map[string]struct{})

		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}

			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}

			if key != strings.ToLower(key) {
				return fmt.Errorf("non-canonical JSON object key %q", key)
			}

			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}

			seen[key] = struct{}{}

			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}

	_, err = decoder.Token()

	return err
}

func validateManifest(manifest Manifest, limits Limits) error {
	if manifest.Schema != SchemaV1Alpha1 {
		return fmt.Errorf("%w: %q", ErrUnsupportedSchema, manifest.Schema)
	}

	if err := validateIdentity("bundle id", manifest.ID, 256); err != nil {
		return err
	}

	if err := validateIdentity("bundle version", manifest.Version, 128); err != nil {
		return err
	}

	if len(manifest.Description) > 4096 {
		return fmt.Errorf("%w: bundle description exceeds 4096 bytes", ErrInvalid)
	}

	if err := validateCapabilities(manifest); err != nil {
		return err
	}

	if err := validateUniqueIdentities("extension", manifest.Extensions); err != nil {
		return err
	}

	if err := validateUniquePaths("skill", manifest.Skills); err != nil {
		return err
	}

	if err := validateUniquePaths("prompt", manifest.Prompts); err != nil {
		return err
	}

	if err := validateAssetSpecs(manifest.Assets); err != nil {
		return err
	}

	if count := len(manifest.Skills) + len(manifest.Prompts) + len(manifest.Assets); count > limits.MaxResources {
		return fmt.Errorf("%w: manifest declares %d resources, maximum is %d", ErrLimitExceeded, count, limits.MaxResources)
	}

	return nil
}

func validateIdentity(field, value string, maxLength int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxLength {
		return fmt.Errorf("%w: %s is empty, non-canonical, or too long", ErrInvalid, field)
	}

	if strings.IndexFunc(value, unicode.IsSpace) >= 0 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: %s %q contains whitespace or control characters", ErrInvalid, field, value)
	}

	return nil
}

func validateCapabilities(manifest Manifest) error {
	seen := make(map[extension.Capability]struct{}, len(manifest.Requires)+len(manifest.Optional))
	for _, capability := range append(slices.Clone(manifest.Requires), manifest.Optional...) {
		if err := validateIdentity("capability", string(capability), 256); err != nil {
			return err
		}

		if _, duplicate := seen[capability]; duplicate {
			return fmt.Errorf("%w: capability %q", ErrDuplicate, capability)
		}

		seen[capability] = struct{}{}
	}

	return nil
}

func validateUniqueIdentities(component string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateIdentity(component, value, 256); err != nil {
			return err
		}

		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%w: %s %q", ErrDuplicate, component, value)
		}

		seen[value] = struct{}{}
	}

	return nil
}

func validateUniquePaths(component string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateLocalPath(value); err != nil {
			return fmt.Errorf("%s path: %w", component, err)
		}

		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%w: %s path %q", ErrDuplicate, component, value)
		}

		seen[value] = struct{}{}
	}

	return nil
}

func validateAssetSpecs(values []AssetSpec) error {
	paths := make(map[string]struct{}, len(values))

	names := make(map[string]struct{}, len(values))
	for _, asset := range values {
		if err := validateIdentity("asset kind", asset.Kind, 128); err != nil {
			return err
		}

		if err := validateIdentity("asset name", asset.Name, 256); err != nil {
			return err
		}

		if err := validateLocalPath(asset.Path); err != nil {
			return fmt.Errorf("asset path: %w", err)
		}

		if len(asset.MediaType) > 256 || strings.IndexFunc(asset.MediaType, unicode.IsControl) >= 0 {
			return fmt.Errorf("%w: invalid asset media type", ErrInvalid)
		}

		if _, duplicate := paths[asset.Path]; duplicate {
			return fmt.Errorf("%w: asset path %q", ErrDuplicate, asset.Path)
		}

		key := asset.Kind + "\x00" + asset.Name
		if _, duplicate := names[key]; duplicate {
			return fmt.Errorf("%w: asset %q/%q", ErrDuplicate, asset.Kind, asset.Name)
		}

		paths[asset.Path] = struct{}{}
		names[key] = struct{}{}
	}

	return nil
}

func validateLocalPath(value string) error {
	if value == "" || value == "." || len(value) > 4096 || !utf8.ValidString(value) ||
		!fs.ValidPath(value) || strings.Contains(value, "\\") || path.Clean(value) != value ||
		strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: path %q is not local and canonical", ErrInvalid, value)
	}

	return nil
}

func filterManifest(manifest Manifest, filters Filters) (Manifest, error) {
	selected := cloneManifest(manifest)

	var err error

	selected.Extensions, err = applyFilter("extension", manifest.Extensions, filters.Extensions)
	if err != nil {
		return Manifest{}, err
	}

	selected.Skills, err = applyFilter("skill", manifest.Skills, filters.Skills)
	if err != nil {
		return Manifest{}, err
	}

	selected.Prompts, err = applyFilter("prompt", manifest.Prompts, filters.Prompts)
	if err != nil {
		return Manifest{}, err
	}

	assetPaths := make([]string, len(manifest.Assets))

	byPath := make(map[string]AssetSpec, len(manifest.Assets))
	for i, asset := range manifest.Assets {
		assetPaths[i] = asset.Path
		byPath[asset.Path] = asset
	}

	selectedPaths, err := applyFilter("asset", assetPaths, filters.Assets)
	if err != nil {
		return Manifest{}, err
	}

	selected.Assets = make([]AssetSpec, len(selectedPaths))
	for i, selectedPath := range selectedPaths {
		selected.Assets[i] = byPath[selectedPath]
	}

	return selected, nil
}

func applyFilter(component string, declared []string, filter ComponentFilter) ([]string, error) {
	available := make(map[string]struct{}, len(declared))
	for _, value := range declared {
		available[value] = struct{}{}
	}

	if err := validateFilterValues(component, "include", filter.Include, available); err != nil {
		return nil, err
	}

	if err := validateFilterValues(component, "exclude", filter.Exclude, available); err != nil {
		return nil, err
	}

	included := available
	if filter.Include != nil {
		included = make(map[string]struct{}, len(filter.Include))
		for _, value := range filter.Include {
			included[value] = struct{}{}
		}
	}

	for _, value := range filter.Exclude {
		delete(included, value)
	}

	selected := make([]string, 0, len(included))
	for _, value := range declared {
		if _, ok := included[value]; ok {
			selected = append(selected, value)
		}
	}

	return selected, nil
}

func filterDiagnostics(manifest, selected Manifest) []Diagnostic {
	diagnostics := make([]Diagnostic, 0)
	diagnostics = appendExcluded(diagnostics, "extension", manifest.Extensions, selected.Extensions)
	diagnostics = appendExcluded(diagnostics, "skill", manifest.Skills, selected.Skills)
	diagnostics = appendExcluded(diagnostics, "prompt", manifest.Prompts, selected.Prompts)

	declaredAssets := make([]string, len(manifest.Assets))

	selectedAssets := make([]string, len(selected.Assets))
	for i, asset := range manifest.Assets {
		declaredAssets[i] = asset.Path
	}

	for i, asset := range selected.Assets {
		selectedAssets[i] = asset.Path
	}

	return appendExcluded(diagnostics, "asset", declaredAssets, selectedAssets)
}

func appendExcluded(diagnostics []Diagnostic, component string, declared, selected []string) []Diagnostic {
	retained := make(map[string]struct{}, len(selected))
	for _, value := range selected {
		retained[value] = struct{}{}
	}

	for _, value := range declared {
		if _, ok := retained[value]; ok {
			continue
		}

		diagnostics = append(diagnostics, Diagnostic{
			Component: component,
			Path:      value,
			Message:   "excluded by selection filter",
		})
	}

	return diagnostics
}

func validateFilterValues(component, operation string, values []string, available map[string]struct{}) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := available[value]; !ok {
			return fmt.Errorf("%w: %s %s %q is not declared by the manifest", ErrInvalid, component, operation, value)
		}

		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%w: %s %s %q", ErrDuplicate, component, operation, value)
		}

		seen[value] = struct{}{}
	}

	return nil
}

func subFS(fsys fs.FS, directory string) (fs.FS, error) {
	if directory == "." {
		return fsys, nil
	}

	return fs.Sub(fsys, directory)
}

func readFileLimit(fsys fs.FS, name string, maximum int64) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	readLimit := maximum
	if maximum < int64(^uint64(0)>>1) {
		readLimit++
	}

	data, err := io.ReadAll(io.LimitReader(file, readLimit))
	if err != nil {
		return nil, err
	}

	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%w: %q exceeds %d bytes", ErrLimitExceeded, name, maximum)
	}

	return data, nil
}

type resourceCounter struct {
	value int
	max   int
}

func (c *resourceCounter) add(path string) error {
	if c.value >= c.max {
		return fmt.Errorf("%w: resource %q exceeds count %d", ErrLimitExceeded, path, c.max)
	}

	c.value++

	return nil
}

func loadSkills(ctx context.Context, fsys fs.FS, roots []string, counter *resourceCounter) ([]harness.Skill, error) {
	values := make([]harness.Skill, 0)

	for _, root := range roots {
		found := 0

		err := walkResource(ctx, fsys, root, func(p string, entry fs.DirEntry) error {
			if entry.Name() != "SKILL.md" {
				return nil
			}

			if err := counter.add(p); err != nil {
				return err
			}

			skill, err := harness.LoadSkillFS(fsys, p)
			if err != nil {
				return err
			}

			values = append(values, skill)
			found++

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("bundle: load skill path %q: %w", root, err)
		}

		if found == 0 {
			return nil, fmt.Errorf("%w: skill path %q contains no SKILL.md", ErrInvalid, root)
		}
	}

	return values, nil
}

func loadPrompts(ctx context.Context, fsys fs.FS, roots []string, counter *resourceCounter) ([]harness.PromptTemplate, error) {
	values := make([]harness.PromptTemplate, 0)

	for _, root := range roots {
		found := 0

		err := walkResource(ctx, fsys, root, func(p string, entry fs.DirEntry) error {
			if entry.Name() == "SKILL.md" || path.Ext(entry.Name()) != ".md" {
				return nil
			}

			if err := counter.add(p); err != nil {
				return err
			}

			template, err := harness.LoadTemplateFS(fsys, p)
			if err != nil {
				return err
			}

			values = append(values, template)
			found++

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("bundle: load prompt path %q: %w", root, err)
		}

		if found == 0 {
			return nil, fmt.Errorf("%w: prompt path %q contains no Markdown template", ErrInvalid, root)
		}
	}

	return values, nil
}

func loadAssets(ctx context.Context, fsys fs.FS, specs []AssetSpec, counter *resourceCounter) ([]extension.Asset, error) {
	values := make([]extension.Asset, 0, len(specs))
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if err := validateRegularResource(ctx, fsys, spec.Path); err != nil {
			return nil, fmt.Errorf("bundle: load asset %q: %w", spec.Path, err)
		}

		if err := counter.add(spec.Path); err != nil {
			return nil, err
		}

		data, err := fs.ReadFile(fsys, spec.Path)
		if err != nil {
			return nil, fmt.Errorf("bundle: read asset %q: %w", spec.Path, err)
		}

		values = append(values, extension.Asset{
			Kind:      spec.Kind,
			Name:      spec.Name,
			MediaType: spec.MediaType,
			Source:    spec.Path,
			Data:      data,
		})
	}

	return values, nil
}

func walkResource(ctx context.Context, fsys fs.FS, root string, visit func(string, fs.DirEntry) error) error {
	return fs.WalkDir(fsys, root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: symbolic link %q is not a declarative resource", ErrInvalid, p)
		}

		if entry.IsDir() {
			return nil
		}

		if !entry.Type().IsRegular() {
			return fmt.Errorf("%w: resource %q is not a regular file", ErrInvalid, p)
		}

		return visit(p, entry)
	})
}

func validateRegularResource(ctx context.Context, fsys fs.FS, name string) error {
	return walkResource(ctx, fsys, name, func(p string, _ fs.DirEntry) error {
		if p != name {
			return fmt.Errorf("%w: asset %q is not a file", ErrInvalid, name)
		}

		return nil
	})
}

func validateLoadedResources(skills []harness.Skill, prompts []harness.PromptTemplate) error {
	if _, err := harness.NewSkillCatalog(skills...); err != nil {
		return fmt.Errorf("%w: validate skills: %w", ErrInvalid, err)
	}

	if err := harness.ValidateTemplates(prompts...); err != nil {
		return fmt.Errorf("%w: validate prompts: %w", ErrInvalid, err)
	}

	return nil
}
