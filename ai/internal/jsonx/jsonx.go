// Package jsonx is the ai module's single JSON encode/decode seam and owns
// bounded request-body extension merging shared by all provider adapters.
//
//nolint:wsl_v5 // Recursive validation and merge guards stay adjacent.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
)

const (
	defaultMaxBytes       = 64 << 10
	defaultMaxDepth       = 12
	defaultMaxNodes       = 2048
	defaultMaxObjectKeys  = 256
	defaultMaxArrayItems  = 512
	defaultMaxKeyBytes    = 256
	defaultMaxStringBytes = 32 << 10
)

// ErrUnsafeExtension means a raw request-body extension is invalid, too
// large, reserved, or collides with an already-typed request field.
var ErrUnsafeExtension = errors.New("json extension: unsafe value")

// MergeLimits bound raw extension work before a request reaches an adapter.
// Zero fields use conservative production defaults.
type MergeLimits struct {
	MaxBytes       int
	MaxDepth       int
	MaxNodes       int
	MaxObjectKeys  int
	MaxArrayItems  int
	MaxKeyBytes    int
	MaxStringBytes int
}

// Marshal encodes v as JSON.
func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// Unmarshal decodes JSON data into v.
func Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// MergeExtraFields recursively adds extra JSON fields to a typed body. It
// never overwrites an existing leaf or array, never mutates either input, and
// rejects reserved dotted paths even when the typed field is absent.
func MergeExtraFields(body any, extra map[string]any, reserved ...string) (any, error) {
	if len(extra) == 0 {
		return body, nil
	}

	return MergeExtraFieldsWithLimits(body, extra, MergeLimits{}, reserved...)
}

// MergeExtraFieldsWithLimits is MergeExtraFields with injectable limits for
// focused tests and fuzzing.
func MergeExtraFieldsWithLimits(
	body any,
	extra map[string]any,
	limits MergeLimits,
	reserved ...string,
) (any, error) {
	limits = resolvedLimits(limits)
	if err := validateRaw(extra, limits); err != nil {
		return nil, err
	}

	bodyMap, err := normalizeObject(body)
	if err != nil {
		return nil, fmt.Errorf("%w: encode typed body", ErrUnsafeExtension)
	}
	extraMap, err := normalizeObject(extra)
	if err != nil {
		return nil, fmt.Errorf("%w: normalize extension", ErrUnsafeExtension)
	}
	reservedSet := make(map[string]struct{}, len(reserved))
	for _, path := range reserved {
		reservedSet[path] = struct{}{}
	}
	if err := checkReserved(extraMap, nil, reservedSet); err != nil {
		return nil, err
	}
	if err := mergeObject(bodyMap, extraMap, nil, reservedSet); err != nil {
		return nil, err
	}

	return bodyMap, nil
}

func checkReserved(value map[string]any, parents []string, reserved map[string]struct{}) error {
	for key, item := range value {
		pathParts := append(append([]string(nil), parents...), key)
		path := strings.Join(pathParts, ".")
		if _, blocked := reserved[path]; blocked {
			return fmt.Errorf("%w: path %q is reserved", ErrUnsafeExtension, path)
		}
		if nested, ok := item.(map[string]any); ok {
			if err := checkReserved(nested, pathParts, reserved); err != nil {
				return err
			}
		}
	}

	return nil
}

func validateRaw(value any, limits MergeLimits) error {
	state := validationState{limits: limits}
	if err := state.visit(value, 1, ""); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > limits.MaxBytes {
		return fmt.Errorf("%w: extension exceeds byte limit", ErrUnsafeExtension)
	}

	return nil
}

type validationState struct {
	limits MergeLimits
	nodes  int
}

//nolint:gocyclo // The type switch is the JSON value grammar and its limits.
func (s *validationState) visit(value any, depth int, path string) error {
	s.nodes++
	if s.nodes > s.limits.MaxNodes {
		return fmt.Errorf("%w: extension exceeds node limit", ErrUnsafeExtension)
	}
	if depth > s.limits.MaxDepth {
		return fmt.Errorf("%w: extension exceeds depth limit", ErrUnsafeExtension)
	}

	switch item := value.(type) {
	case nil, bool:
		return nil
	case string:
		if len(item) > s.limits.MaxStringBytes {
			return fmt.Errorf("%w: path %q exceeds string limit", ErrUnsafeExtension, path)
		}
		return nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return nil
	case float32:
		if math.IsNaN(float64(item)) || math.IsInf(float64(item), 0) {
			return fmt.Errorf("%w: path %q contains non-finite number", ErrUnsafeExtension, path)
		}
		return nil
	case float64:
		if math.IsNaN(item) || math.IsInf(item, 0) {
			return fmt.Errorf("%w: path %q contains non-finite number", ErrUnsafeExtension, path)
		}
		return nil
	case map[string]any:
		if len(item) > s.limits.MaxObjectKeys {
			return fmt.Errorf("%w: path %q exceeds object-key limit", ErrUnsafeExtension, path)
		}
		for key, nested := range item {
			if len(key) == 0 || len(key) > s.limits.MaxKeyBytes {
				return fmt.Errorf("%w: path %q contains an invalid key", ErrUnsafeExtension, path)
			}
			child := joinPath(path, key)
			if credentialShaped(key) {
				return fmt.Errorf("%w: path %q is credential-shaped", ErrUnsafeExtension, child)
			}
			if err := s.visit(nested, depth+1, child); err != nil {
				return err
			}
		}
		return nil
	case []any:
		if len(item) > s.limits.MaxArrayItems {
			return fmt.Errorf("%w: path %q exceeds array limit", ErrUnsafeExtension, path)
		}
		for _, nested := range item {
			if err := s.visit(nested, depth+1, path); err != nil {
				return err
			}
		}
		return nil
	case time.Time, time.Duration:
		return fmt.Errorf("%w: path %q contains a non-JSON type", ErrUnsafeExtension, path)
	default:
		kind := reflect.TypeOf(value)
		if kind == nil {
			return nil
		}
		return fmt.Errorf("%w: path %q contains unsupported type %s", ErrUnsafeExtension, path, kind.Kind())
	}
}

func normalizeObject(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}

	return result, nil
}

func mergeObject(
	target map[string]any,
	extra map[string]any,
	parents []string,
	reserved map[string]struct{},
) error {
	for key, value := range extra {
		pathParts := append(append([]string(nil), parents...), key)
		path := strings.Join(pathParts, ".")
		if _, blocked := reserved[path]; blocked {
			return fmt.Errorf("%w: path %q is reserved", ErrUnsafeExtension, path)
		}

		existing, exists := target[key]
		if !exists {
			target[key] = value
			continue
		}
		existingMap, existingOK := existing.(map[string]any)
		extraMap, extraOK := value.(map[string]any)
		if !existingOK || !extraOK {
			return fmt.Errorf("%w: path %q collides with a typed field", ErrUnsafeExtension, path)
		}
		if err := mergeObject(existingMap, extraMap, pathParts, reserved); err != nil {
			return err
		}
	}

	return nil
}

func resolvedLimits(value MergeLimits) MergeLimits {
	if value.MaxBytes == 0 {
		value.MaxBytes = defaultMaxBytes
	}
	if value.MaxDepth == 0 {
		value.MaxDepth = defaultMaxDepth
	}
	if value.MaxNodes == 0 {
		value.MaxNodes = defaultMaxNodes
	}
	if value.MaxObjectKeys == 0 {
		value.MaxObjectKeys = defaultMaxObjectKeys
	}
	if value.MaxArrayItems == 0 {
		value.MaxArrayItems = defaultMaxArrayItems
	}
	if value.MaxKeyBytes == 0 {
		value.MaxKeyBytes = defaultMaxKeyBytes
	}
	if value.MaxStringBytes == 0 {
		value.MaxStringBytes = defaultMaxStringBytes
	}

	return value
}

func credentialShaped(key string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(key))
	switch normalized {
	case "apikey", "authorization", "auth", "password", "secret", "token", "accesstoken", "refreshtoken", "bearertoken", "privatekey", "clientsecret":
		return true
	default:
		return strings.HasSuffix(normalized, "apikey") ||
			strings.HasSuffix(normalized, "password") ||
			strings.HasSuffix(normalized, "secret") ||
			strings.HasSuffix(normalized, "token")
	}
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}

	return parent + "." + key
}
