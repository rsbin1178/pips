package resource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rsbin/pips/agent/bundle"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/internal/coding/paths"
)

const (
	priorityExtension = iota
	priorityUser
	priorityProject
)

type discoveryBudget struct {
	entries int
	skills  int
	bundles int
}

// Load discovers and fully decodes scoped Skills and Bundles. Project paths
// are not inspected unless ProjectTrusted is true.
func Load(ctx context.Context, options Options) (Result, error) {
	if err := validateOptions(options); err != nil {
		return Result{}, err
	}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	budget := &discoveryBudget{}
	contentBudget := &readBudget{maximum: options.Limits.MaxTotalSkillBytes}

	userSkills, err := loadUserSkills(ctx, options, budget, contentBudget)
	if err != nil {
		return Result{}, err
	}

	userBundles, err := loadUserBundles(ctx, options, budget)
	if err != nil {
		return Result{}, err
	}

	var (
		projectSkills  []skillEntry
		projectBundles []*bundle.Bundle
	)

	if options.ProjectTrusted {
		projectSkills, err = loadProjectSkills(ctx, options, budget, contentBudget)
		if err != nil {
			return Result{}, err
		}

		projectBundles, err = loadProjectBundles(ctx, options, budget)
		if err != nil {
			return Result{}, err
		}
	}

	direct := slices.Concat(userSkills, projectSkills)

	direct, diagnostics, err := mergeSkillEntries(direct, nil)
	if err != nil {
		return Result{}, err
	}

	bundles := slices.Concat(userBundles, projectBundles)

	bundleDiagnostics, err := validateBundles(bundles)
	if err != nil {
		return Result{}, err
	}

	diagnostics = append(diagnostics, bundleDiagnostics...)
	slices.SortFunc(diagnostics, compareDiagnostic)

	return Result{direct: direct, bundles: bundles, diagnostics: diagnostics}, nil
}

func validateOptions(options Options) error {
	if options.Paths.Root() == "" {
		return fmt.Errorf("%w: empty user path layout", ErrInvalid)
	}

	if options.ProjectTrusted && options.Tree == nil {
		return fmt.Errorf("%w: trusted project requires a workspace tree", ErrInvalid)
	}

	if err := validateLimits(options.Limits); err != nil {
		return err
	}

	return nil
}

func validateLimits(limits Limits) error {
	if limits.MaxEntries <= 0 || limits.MaxSkillManifests <= 0 ||
		limits.MaxBundleManifests <= 0 || limits.MaxDepth <= 0 ||
		limits.MaxPathBytes <= 0 || limits.MaxSkillBytes <= 0 ||
		limits.MaxTotalSkillBytes <= 0 {
		return fmt.Errorf("%w: every discovery limit must be positive", ErrInvalid)
	}

	bundleLimits := limits.Bundle
	if bundleLimits.MaxManifestBytes <= 0 || bundleLimits.MaxResourceBytes <= 0 ||
		bundleLimits.MaxTotalBytes <= 0 || bundleLimits.MaxResources <= 0 {
		return fmt.Errorf("%w: every Bundle limit must be positive", ErrInvalid)
	}

	return nil
}

func loadUserSkills(
	ctx context.Context,
	options Options,
	budget *discoveryBudget,
	contentBudget *readBudget,
) ([]skillEntry, error) {
	fsys, closeFS, exists, err := openUserDirectory(options.Paths.SkillsDir())
	if err != nil || !exists {
		return nil, err
	}
	defer func() { _ = closeFS() }()

	return loadDirectSkills(
		ctx,
		fsys,
		"user:skills/",
		priorityUser,
		options.Limits,
		budget,
		contentBudget,
	)
}

func loadProjectSkills(
	ctx context.Context,
	options Options,
	budget *discoveryBudget,
	contentBudget *readBudget,
) ([]skillEntry, error) {
	fys, exists, err := projectSubFS(options, paths.ProjectSkillsDir())
	if err != nil || !exists {
		return nil, err
	}

	return loadDirectSkills(
		ctx,
		fys,
		"project:"+paths.ProjectSkillsDir()+"/",
		priorityProject,
		options.Limits,
		budget,
		contentBudget,
	)
}

func loadDirectSkills(
	ctx context.Context,
	fys fs.FS,
	provenancePrefix string,
	priority int,
	limits Limits,
	budget *discoveryBudget,
	contentBudget *readBudget,
) ([]skillEntry, error) {
	manifestPaths, err := discoverSkillManifests(ctx, fys, limits, budget)
	if err != nil {
		return nil, err
	}

	bounded := boundedFS{fsys: fys, budget: contentBudget, maxFile: limits.MaxSkillBytes}
	entries := make([]skillEntry, 0, len(manifestPaths))

	for _, manifestPath := range manifestPaths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		skill, err := harness.LoadSkillFS(bounded, manifestPath)
		if err != nil {
			return nil, fmt.Errorf("coding resource: load Skill %q: %w", manifestPath, err)
		}

		provenance := provenancePrefix + manifestPath
		skill.Source = provenance
		skill.AllowedTools = nil

		entries = append(entries, skillEntry{
			skill:      skill,
			provenance: provenance,
			priority:   priority,
		})
	}

	return entries, nil
}

func discoverSkillManifests(
	ctx context.Context,
	fys fs.FS,
	limits Limits,
	budget *discoveryBudget,
) ([]string, error) {
	var manifests []string

	err := fs.WalkDir(fys, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		budget.entries++
		if budget.entries > limits.MaxEntries {
			return fmt.Errorf("%w: more than %d filesystem entries", ErrLimitExceeded, limits.MaxEntries)
		}

		if len(name) > limits.MaxPathBytes {
			return fmt.Errorf("%w: resource path exceeds %d bytes", ErrLimitExceeded, limits.MaxPathBytes)
		}

		if name != "." && strings.Count(name, "/")+1 > limits.MaxDepth {
			return fmt.Errorf("%w: resource path %q exceeds depth %d", ErrLimitExceeded, name, limits.MaxDepth)
		}

		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}

		budget.skills++
		if budget.skills > limits.MaxSkillManifests {
			return fmt.Errorf("%w: more than %d Skill manifests", ErrLimitExceeded, limits.MaxSkillManifests)
		}

		manifests = append(manifests, name)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("coding resource: discover Skills: %w", err)
	}

	slices.Sort(manifests)

	return manifests, nil
}

func loadUserBundles(
	ctx context.Context,
	options Options,
	budget *discoveryBudget,
) ([]*bundle.Bundle, error) {
	fys, closeFS, exists, err := openUserDirectory(options.Paths.BundlesDir())
	if err != nil || !exists {
		return nil, err
	}
	defer func() { _ = closeFS() }()

	return loadBundles(ctx, fys, bundle.ScopeUser, bundle.TrustApproved, options.Limits, budget)
}

func loadProjectBundles(
	ctx context.Context,
	options Options,
	budget *discoveryBudget,
) ([]*bundle.Bundle, error) {
	fys, exists, err := projectSubFS(options, paths.ProjectBundlesDir())
	if err != nil || !exists {
		return nil, err
	}

	return loadBundles(ctx, fys, bundle.ScopeProject, bundle.TrustApproved, options.Limits, budget)
}

func loadBundles(
	ctx context.Context,
	fys fs.FS,
	scope bundle.Scope,
	trust bundle.TrustDecision,
	limits Limits,
	budget *discoveryBudget,
) ([]*bundle.Bundle, error) {
	manifestPaths, err := discoverBundleManifests(fys, limits, budget)
	if err != nil {
		return nil, err
	}

	loader, err := bundle.New(fys, bundle.WithLimits(limits.Bundle))
	if err != nil {
		return nil, err
	}

	values := make([]*bundle.Bundle, 0, len(manifestPaths))
	for _, manifestPath := range manifestPaths {
		value, loadErr := loader.Load(ctx, manifestPath, bundle.Settings{Scope: scope, Trust: trust})
		if loadErr != nil {
			return nil, fmt.Errorf("coding resource: load Bundle %q: %w", manifestPath, loadErr)
		}

		values = append(values, value)
	}

	return values, nil
}

func discoverBundleManifests(
	fys fs.FS,
	limits Limits,
	budget *discoveryBudget,
) ([]string, error) {
	entries, err := fs.ReadDir(fys, ".")
	if err != nil {
		return nil, fmt.Errorf("coding resource: discover Bundles: %w", err)
	}

	manifestPaths := make([]string, 0, len(entries))
	for _, entry := range entries {
		budget.entries++
		if budget.entries > limits.MaxEntries {
			return nil, fmt.Errorf("%w: more than %d filesystem entries", ErrLimitExceeded, limits.MaxEntries)
		}

		if !entry.IsDir() {
			continue
		}

		manifestPath := path.Join(entry.Name(), bundle.DefaultManifestPath)
		if len(manifestPath) > limits.MaxPathBytes {
			return nil, fmt.Errorf("%w: Bundle path exceeds %d bytes", ErrLimitExceeded, limits.MaxPathBytes)
		}

		info, statErr := fs.Stat(fys, manifestPath)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}

		if statErr != nil {
			return nil, fmt.Errorf("coding resource: inspect Bundle manifest %q: %w", manifestPath, statErr)
		}

		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: Bundle manifest %q is not regular", ErrInvalid, manifestPath)
		}

		budget.bundles++
		if budget.bundles > limits.MaxBundleManifests {
			return nil, fmt.Errorf("%w: more than %d Bundle manifests", ErrLimitExceeded, limits.MaxBundleManifests)
		}

		manifestPaths = append(manifestPaths, manifestPath)
	}

	slices.Sort(manifestPaths)

	return manifestPaths, nil
}

func openUserDirectory(directory string) (fs.FS, func() error, bool, error) {
	resolved, err := filepathEvalSymlinks(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false, nil
	}

	if err != nil {
		return nil, nil, false, fmt.Errorf("coding resource: resolve user root: %w", err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, nil, false, fmt.Errorf("coding resource: inspect user root: %w", err)
	}

	if !info.IsDir() {
		return nil, nil, false, fmt.Errorf("%w: user root is not a directory", ErrInvalid)
	}

	if info.Mode().Perm()&0o022 != 0 {
		return nil, nil, false, fmt.Errorf("%w: user root is group/other writable", ErrInsecure)
	}

	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, nil, false, fmt.Errorf("coding resource: open user root: %w", err)
	}

	openedInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()

		return nil, nil, false, fmt.Errorf("coding resource: verify user root: %w", err)
	}

	if !os.SameFile(info, openedInfo) {
		_ = root.Close()

		return nil, nil, false, errors.New("coding resource: user root changed while opening")
	}

	return root.FS(), root.Close, true, nil
}

func projectSubFS(options Options, directory string) (fs.FS, bool, error) {
	projectFS := options.Tree.FileSystem()

	info, err := fs.Stat(projectFS, directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("coding resource: inspect project root %q: %w", directory, err)
	}

	if !info.IsDir() {
		return nil, false, fmt.Errorf("%w: project root %q is not a directory", ErrInvalid, directory)
	}

	sub, err := fs.Sub(projectFS, directory)
	if err != nil {
		return nil, false, fmt.Errorf("coding resource: open project root %q: %w", directory, err)
	}

	return sub, true, nil
}

func validateBundles(values []*bundle.Bundle) ([]Diagnostic, error) {
	bundleIDs := make(map[string]string, len(values))
	extensionIDs := make(map[string]string)
	skillNames := make(map[string]string)
	promptNames := make(map[string]string)

	var diagnostics []Diagnostic

	for _, value := range values {
		manifest := value.Manifest()
		owner := string(value.Scope()) + ":bundle:" + manifest.ID

		if existing, duplicate := bundleIDs[manifest.ID]; duplicate {
			return nil, duplicateResource("Bundle", manifest.ID, existing, owner)
		}

		bundleIDs[manifest.ID] = owner

		for _, extensionID := range value.ExtensionIDs() {
			if existing, duplicate := extensionIDs[extensionID]; duplicate {
				return nil, duplicateResource("Extension", extensionID, existing, owner)
			}

			extensionIDs[extensionID] = owner
		}

		for _, skill := range value.Skills() {
			if existing, duplicate := skillNames[skill.Name]; duplicate {
				return nil, duplicateResource("Skill", skill.Name, existing, owner)
			}

			skillNames[skill.Name] = owner
		}

		for _, prompt := range value.Prompts() {
			if existing, duplicate := promptNames[prompt.Name]; duplicate {
				return nil, duplicateResource("prompt", prompt.Name, existing, owner)
			}

			promptNames[prompt.Name] = owner
		}

		for _, diagnostic := range value.Diagnostics() {
			diagnostics = append(diagnostics, Diagnostic{
				Code:     "bundle_diagnostic",
				Resource: owner,
				Message:  diagnostic.Component + ": " + diagnostic.Message,
			})
		}
	}

	return diagnostics, nil
}

func duplicateResource(kind, name, first, second string) error {
	return fmt.Errorf("%w: %s %q from %q and %q", ErrDuplicate, kind, name, first, second)
}

func filepathEvalSymlinks(name string) (string, error) {
	return filepath.EvalSymlinks(name)
}
