//nolint:wsl_v5 // Protocol loading keeps ordered validation and recovery steps adjacent.
package agentplugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rsbin/pips/agent/extension"
	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/paths"
)

type discoveryRoot struct {
	directory  string
	provenance string
}

// Load discovers immediate plugin package directories from the user root and,
// only when trusted, the project root. Each package is then loaded according
// to the Agent Plugins 1.0.0 fixed-location contract.
func Load(ctx context.Context, options Options) (Result, error) {
	if err := validateOptions(options); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	roots := []discoveryRoot{{directory: options.Paths.PluginsDir(), provenance: "user"}}
	if options.ProjectTrusted {
		roots = append(roots, discoveryRoot{
			directory:  filepath.Join(options.Tree.Path(), filepath.FromSlash(paths.ProjectPluginsDir())),
			provenance: "project",
		})
	}

	var (
		packages    []Package
		skills      = make([]extension.SkillEntry, 0)
		definitions []codingmcp.Definition
		diagnostics []Diagnostic
		serverIDs   = make(map[string]struct{})
	)
	for _, root := range roots {
		entries, rootDiagnostics, err := discoverPackages(ctx, root, options.Limits)
		if err != nil {
			return Result{}, err
		}
		diagnostics = append(diagnostics, rootDiagnostics...)
		for _, entry := range entries {
			if len(packages) >= options.Limits.MaxPlugins {
				return Result{}, fmt.Errorf("%w: more than %d packages", ErrLimitExceeded, options.Limits.MaxPlugins)
			}

			loaded, err := loadPackage(ctx, entry, options.Paths.PluginDataDir(), options.Limits)
			if err != nil {
				diagnostics = append(diagnostics, loaded.diagnostics...)
				diagnostics = append(diagnostics, Diagnostic{
					Plugin: entry.provenance, Component: "manifest", Code: "plugin_rejected",
					Message: err.Error(),
				})
				continue
			}
			acceptedDefinitions := make([]codingmcp.Definition, 0, len(loaded.definitions))
			for _, definition := range loaded.definitions {
				if len(definitions) >= options.Limits.MaxMCPServers {
					diagnostics = append(diagnostics, Diagnostic{
						Plugin: entry.provenance, Component: "mcp", Code: "component_limit",
						Message: "MCP servers beyond the client-wide limit were ignored",
					})
					break
				}
				if _, duplicate := serverIDs[definition.ID]; duplicate {
					diagnostics = append(diagnostics, Diagnostic{
						Plugin: entry.provenance, Component: "mcp", Code: "server_duplicate",
						Message: "a duplicate resolved MCP server was ignored",
					})
					continue
				}
				serverIDs[definition.ID] = struct{}{}
				acceptedDefinitions = append(acceptedDefinitions, definition)
				definitions = append(definitions, definition)
			}
			loaded.pkg.ServerCount = len(acceptedDefinitions)
			packages = append(packages, loaded.pkg)
			skills = append(skills, loaded.skills...)
			diagnostics = append(diagnostics, loaded.diagnostics...)
		}
	}

	definitionSet, err := codingmcp.NewDefinitions(definitions, options.Limits.MaxMCPServers)
	if err != nil {
		return Result{}, fmt.Errorf("agent plugin: build MCP definitions: %w", err)
	}
	slices.SortFunc(diagnostics, compareDiagnostic)

	return Result{
		packages: packages, skills: skills,
		definitions: definitionSet, diagnostics: diagnostics,
	}, nil
}

// LoadDirectory validates and decodes one explicit Agent Plugin package root.
// It is intended for client validation surfaces; runtime discovery uses Load.
//
//nolint:gocyclo // Absolute-path, limit, resolution, and projection checks are one validation boundary.
func LoadDirectory(
	ctx context.Context,
	directory string,
	dataBase string,
	limits Limits,
) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !filepath.IsAbs(directory) || !filepath.IsAbs(dataBase) {
		return Result{}, fmt.Errorf("%w: package and data paths must be absolute", ErrInvalid)
	}
	if limits.MaxPlugins <= 0 || limits.MaxManifestBytes <= 0 || limits.MaxMCPBytes <= 0 ||
		limits.MaxMCPServers <= 0 || limits.MaxSkills <= 0 || limits.MaxSkillBytes <= 0 ||
		limits.MaxResourcesPerSkill <= 0 || limits.MaxResourceBytes <= 0 ||
		limits.MaxTotalResourceBytes <= 0 {
		return Result{}, fmt.Errorf("%w: every limit must be positive", ErrInvalid)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return Result{}, fmt.Errorf("%w: resolve package root", ErrInvalid)
	}
	loaded, err := loadPackage(ctx, packageLocation{
		root: resolved, instance: filepath.Clean(directory),
		provenance: "directory:" + filepath.Base(directory),
	}, dataBase, limits)
	if err != nil {
		slices.SortFunc(loaded.diagnostics, compareDiagnostic)

		return Result{diagnostics: loaded.diagnostics}, err
	}
	definitions, err := codingmcp.NewDefinitions(loaded.definitions, limits.MaxMCPServers)
	if err != nil {
		return Result{}, err
	}
	slices.SortFunc(loaded.diagnostics, compareDiagnostic)

	return Result{
		packages: []Package{loaded.pkg}, skills: loaded.skills,
		definitions: definitions, diagnostics: loaded.diagnostics,
	}, nil
}

type packageLocation struct {
	root       string
	instance   string
	provenance string
}

func discoverPackages(
	ctx context.Context,
	root discoveryRoot,
	limits Limits,
) ([]packageLocation, []Diagnostic, error) {
	info, err := os.Lstat(root.directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("agent plugin: inspect %s root: %w", root.provenance, err)
	}
	if !info.IsDir() {
		return nil, []Diagnostic{{
			Plugin: root.provenance, Component: "discovery", Code: "root_invalid",
			Message: "plugin discovery root is not a directory",
		}}, nil
	}

	entries, err := os.ReadDir(root.directory)
	if err != nil {
		return nil, nil, fmt.Errorf("agent plugin: read %s root: %w", root.provenance, err)
	}
	result := make([]packageLocation, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		candidate := filepath.Join(root.directory, entry.Name())
		resolved, valid := resolvePackageCandidate(candidate)
		if !valid {
			continue
		}
		result = append(result, packageLocation{
			root: resolved, instance: filepath.Clean(candidate),
			provenance: root.provenance + ":" + entry.Name(),
		})
		if len(result) > limits.MaxPlugins {
			return nil, nil, fmt.Errorf("%w: more than %d packages", ErrLimitExceeded, limits.MaxPlugins)
		}
	}

	slices.SortFunc(result, func(left, right packageLocation) int {
		return strings.Compare(left.provenance, right.provenance)
	})

	return result, nil, nil
}

func resolvePackageCandidate(candidate string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", false
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return "", false
	}
	defer func() { _ = root.Close() }()
	manifest, err := root.Stat("plugin.json")

	return resolved, err == nil && manifest.Mode().IsRegular()
}

type loadedPackage struct {
	pkg         Package
	skills      []extension.SkillEntry
	definitions []codingmcp.Definition
	diagnostics []Diagnostic
}

func loadPackage(
	ctx context.Context,
	location packageLocation,
	dataBase string,
	limits Limits,
) (loadedPackage, error) {
	root, err := os.OpenRoot(location.root)
	if err != nil {
		return loadedPackage{}, fmt.Errorf("%w: open filesystem-resolved root", ErrInvalid)
	}
	defer func() { _ = root.Close() }()

	manifestData, err := readRootFile(root, "plugin.json", limits.MaxManifestBytes)
	if err != nil {
		return loadedPackage{}, err
	}
	manifest, diagnostics, err := decodeManifest(manifestData)
	for index := range diagnostics {
		diagnostics[index].Plugin = location.provenance
	}
	if err != nil {
		return loadedPackage{diagnostics: diagnostics}, err
	}

	dataDirectory := pluginDataPath(dataBase, location.instance)
	pkg := Package{
		Manifest: manifest, Provenance: location.provenance,
		Root: location.root, Data: dataDirectory, instance: location.instance,
	}
	skills, skillDiagnostics := loadSkills(ctx, root.FS(), pkg, limits)
	definitions, mcpDiagnostics := loadMCP(ctx, root, pkg, limits)
	pkg.SkillCount = len(skills)
	pkg.ServerCount = len(definitions)
	diagnostics = append(diagnostics, skillDiagnostics...)
	diagnostics = append(diagnostics, mcpDiagnostics...)

	return loadedPackage{
		pkg: pkg, skills: skills, definitions: definitions, diagnostics: diagnostics,
	}, nil
}

func readRootFile(root *os.Root, name string, maximum int64) ([]byte, error) {
	info, err := root.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect %s: %w", ErrInvalid, name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInvalid, name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s", ErrInvalid, name)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s", ErrInvalid, name)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrLimitExceeded, name, maximum)
	}

	return data, nil
}

//nolint:gocyclo // Every positive bound is intentionally validated at the public boundary.
func validateOptions(options Options) error {
	if options.Paths.Root() == "" || options.Paths.PluginsDir() == "" ||
		options.Paths.PluginDataDir() == "" {
		return fmt.Errorf("%w: incomplete path layout", ErrInvalid)
	}
	if options.ProjectTrusted && options.Tree == nil {
		return fmt.Errorf("%w: trusted project requires a workspace tree", ErrInvalid)
	}
	limits := options.Limits
	if limits.MaxPlugins <= 0 || limits.MaxManifestBytes <= 0 || limits.MaxMCPBytes <= 0 ||
		limits.MaxMCPServers <= 0 || limits.MaxSkills <= 0 || limits.MaxSkillBytes <= 0 ||
		limits.MaxResourcesPerSkill <= 0 || limits.MaxResourceBytes <= 0 ||
		limits.MaxTotalResourceBytes <= 0 {
		return fmt.Errorf("%w: every limit must be positive", ErrInvalid)
	}

	return nil
}
