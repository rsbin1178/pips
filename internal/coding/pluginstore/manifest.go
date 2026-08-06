//nolint:wsl_v5 // Strict decoding and filesystem identity checks are intentionally adjacent.
package pluginstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rsbin/pips/internal/jsonx"
)

// ParseManifest decodes and validates one complete non-executing manifest.
// It rejects duplicate keys, non-canonical key casing, unknown fields,
// trailing values, unsafe paths, and out-of-budget metadata.
func ParseManifest(data []byte) (PluginManifest, error) {
	return parseManifest(data, DefaultLimits())
}

func parseManifest(data []byte, limits Limits) (PluginManifest, error) {
	if int64(len(data)) > limits.ManifestBytes {
		return PluginManifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrLimitExceeded, limits.ManifestBytes)
	}
	if err := rejectNonCanonicalJSON(data); err != nil {
		return PluginManifest{}, err
	}
	var manifest PluginManifest
	if err := jsonx.Decode(data, &manifest); err != nil {
		return PluginManifest{}, fmt.Errorf("%w: decode manifest: %w", ErrInvalid, err)
	}
	if err := manifest.validate(limits); err != nil {
		return PluginManifest{}, err
	}
	return cloneManifest(manifest), nil
}

// ReadManifest reads one regular, non-symlink manifest file without executing
// or invoking anything from its containing package.
func ReadManifest(manifestPath string) (PluginManifest, []byte, error) {
	limits := DefaultLimits()
	data, err := readStableRegularFile(manifestPath, limits.ManifestBytes, ErrInvalid)
	if err != nil {
		return PluginManifest{}, nil, err
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		return PluginManifest{}, nil, err
	}
	return manifest, data, nil
}

// SelectTarget returns the exact artifact declared for goos/goarch.
func (m PluginManifest) SelectTarget(goos, goarch string) (TargetArtifact, error) {
	return m.selectTarget(goos, goarch, DefaultLimits())
}

func (m PluginManifest) selectTarget(goos, goarch string, limits Limits) (TargetArtifact, error) {
	if err := m.validate(limits); err != nil {
		return TargetArtifact{}, err
	}
	if !validSupportedTarget(TargetPlatform{OS: goos, Arch: goarch}) {
		return TargetArtifact{}, fmt.Errorf("%w: target %s/%s", ErrUnsupportedTarget, goos, goarch)
	}
	for _, target := range m.Targets {
		if target.OS == goos && target.Arch == goarch {
			return target, nil
		}
	}
	return TargetArtifact{}, fmt.Errorf("%w: target %s/%s is not declared", ErrUnsupportedTarget, goos, goarch)
}

func rejectNonCanonicalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("%w: manifest JSON: %w", ErrInvalid, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: manifest contains multiple JSON values", ErrInvalid)
		}
		return fmt.Errorf("%w: manifest JSON: %w", ErrInvalid, err)
	}
	return nil
}

//nolint:gocyclo // Recursive strict JSON scanning keeps key and delimiter validation together.
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
				return errors.New("object key is not a string")
			}
			if key != strings.ToLower(key) {
				return fmt.Errorf("non-canonical object key %q", key)
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
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
		return errors.New("unexpected JSON delimiter")
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	expected := json.Delim('}')
	if delimiter == '[' {
		expected = ']'
	}
	if closing != expected {
		return errors.New("mismatched JSON delimiter")
	}
	return nil
}

//nolint:gocyclo // Identity, size, and replacement checks form one read boundary.
func readStableRegularFile(filePath string, limit int64, class error) ([]byte, error) {
	if strings.TrimSpace(filePath) == "" || strings.ContainsRune(filePath, '\x00') {
		return nil, fmt.Errorf("%w: empty or NUL path", class)
	}
	absolute, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve path: %w", class, err)
	}
	if err := rejectSymlinkComponents(filepath.Dir(absolute), absolute); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect file: %w", class, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: file must be a regular non-symlink", class)
	}
	file, err := os.Open(absolute) //nolint:gosec // Lstat/open identity is checked below.
	if err != nil {
		return nil, fmt.Errorf("%w: open file: %w", class, err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("%w: file changed while opening", ErrConflict)
	}
	if limit < 1 || limit == int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("%w: invalid read limit", ErrInvalid)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read file: %w", class, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: file exceeds %d bytes", ErrLimitExceeded, limit)
	}
	if err := rejectSymlinkComponents(filepath.Dir(absolute), absolute); err != nil {
		return nil, err
	}
	after, err := os.Lstat(absolute)
	if err != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, after) {
		return nil, fmt.Errorf("%w: file changed while reading", ErrConflict)
	}
	return data, nil
}
