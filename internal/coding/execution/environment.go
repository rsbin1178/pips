package execution

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	homeEnvironment = "HOME"
	langEnvironment = "LANG"
	pathEnvironment = "PATH"
)

var inheritedEnvironmentKeys = []string{
	homeEnvironment,
	langEnvironment,
	"LC_ALL",
	"LC_COLLATE",
	"LC_CTYPE",
	"LC_MESSAGES",
	"LC_MONETARY",
	"LC_NUMERIC",
	"LC_TIME",
	"LOGNAME",
	"NO_COLOR",
	pathEnvironment,
	"TERM",
	"USER",
}

func environmentSnapshot(
	lookup func(string) (string, bool),
	privateDir string,
	overrides []EnvVar,
) ([]string, error) {
	if lookup == nil {
		return nil, fmt.Errorf("%w: environment lookup is required", ErrInvalidOperation)
	}

	values := make(map[string]string, len(inheritedEnvironmentKeys)+len(overrides)+4)
	for _, name := range inheritedEnvironmentKeys {
		value, ok := lookup(name)
		if !ok || !validPlainText(value, true) {
			continue
		}

		values[name] = value
	}

	for _, variable := range overrides {
		values[variable.Name] = variable.Value
	}

	if path, ok := values[pathEnvironment]; ok {
		if cleaned := cleanExecutablePath(path); cleaned != "" {
			values[pathEnvironment] = cleaned
		} else {
			delete(values, pathEnvironment)
		}
	}

	privateValues, err := preparePrivateEnvironment(privateDir)
	if err != nil {
		return nil, err
	}

	maps.Copy(values, privateValues)

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}

	sort.Strings(names)

	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}

	return environment, nil
}

func cleanExecutablePath(value string) string {
	entries := filepath.SplitList(value)
	cleaned := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))

	for _, entry := range entries {
		if !filepath.IsAbs(entry) || !validPlainText(entry, false) {
			continue
		}

		entry = filepath.Clean(entry)
		if _, exists := seen[entry]; exists {
			continue
		}

		seen[entry] = struct{}{}
		cleaned = append(cleaned, entry)
	}

	return strings.Join(cleaned, string(os.PathListSeparator))
}

func preparePrivateEnvironment(privateDir string) (map[string]string, error) {
	paths := map[string]string{
		"GOCACHE":        filepath.Join(privateDir, "go-cache"),
		"GOTMPDIR":       filepath.Join(privateDir, "go-tmp"),
		"TMPDIR":         filepath.Join(privateDir, "tmp"),
		"XDG_CACHE_HOME": filepath.Join(privateDir, "cache"),
	}

	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		if err := os.Mkdir(paths[name], 0o700); err != nil {
			return nil, fmt.Errorf("coding execution: create private %s: %w", strings.ToLower(name), err)
		}
	}

	return paths, nil
}
