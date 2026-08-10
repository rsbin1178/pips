package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
)

// PortSchema is an immutable JSON Schema draft 2020-12 contract. External
// references are rejected because schemas are resolved without a loader.
type PortSchema struct {
	raw         []byte
	resolved    *jsonschema.Resolved
	fingerprint string
}

// ParsePortSchema validates, resolves, and snapshots a JSON Schema.
func ParsePortSchema(data []byte) (PortSchema, error) {
	var schema jsonschema.Schema
	if err := decodeJSON(data, &schema, defaultJSONLimits, false); err != nil {
		return PortSchema{}, fmt.Errorf("%w: decode: %w", ErrInvalidSchema, err)
	}

	resolved, err := schema.Resolve(nil)
	if err != nil {
		return PortSchema{}, fmt.Errorf("%w: resolve: %w", ErrInvalidSchema, err)
	}

	canonical, err := json.Marshal(&schema)
	if err != nil {
		return PortSchema{}, fmt.Errorf("%w: encode canonical schema: %w", ErrInvalidSchema, err)
	}

	digest := sha256.Sum256(canonical)

	return PortSchema{
		raw:         canonical,
		resolved:    resolved,
		fingerprint: hex.EncodeToString(digest[:]),
	}, nil
}

// SchemaFor derives a PortSchema from T using encoding/json field names.
func SchemaFor[T any]() (PortSchema, error) {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		return PortSchema{}, fmt.Errorf("%w: derive schema for %T: %w", ErrInvalidSchema, *new(T), err)
	}

	data, err := json.Marshal(schema)
	if err != nil {
		return PortSchema{}, fmt.Errorf("%w: encode schema for %T: %w", ErrInvalidSchema, *new(T), err)
	}

	return ParsePortSchema(data)
}

// IsValid reports whether s contains a resolved schema.
func (s PortSchema) IsValid() bool {
	return len(s.raw) > 0 && s.resolved != nil && s.fingerprint != ""
}

// RawJSON returns an independent canonical JSON representation.
func (s PortSchema) RawJSON() []byte {
	return slices.Clone(s.raw)
}

// Fingerprint returns the SHA-256 digest of the canonical schema.
func (s PortSchema) Fingerprint() string {
	return s.fingerprint
}

// Equal reports whether two schemas have the same canonical representation.
func (s PortSchema) Equal(other PortSchema) bool {
	return s.fingerprint != "" && s.fingerprint == other.fingerprint
}

// Validate checks value against s.
func (s PortSchema) Validate(value Value) (returnErr error) {
	if !s.IsValid() {
		return ErrInvalidSchema
	}

	if !value.IsValid() {
		return ErrInvalidValue
	}

	// jsonschema-go currently classifies json.Number by its underlying string
	// kind during type validation. Decode this private validation view with the
	// standard float64 representation while Value keeps its number-preserving
	// immutable snapshot for storage and bindings.
	var instance any
	if err := json.Unmarshal(value.raw, &instance); err != nil {
		return fmt.Errorf("%w: decode validation value: %w", ErrInvalidValue, err)
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("%w: validator panicked", ErrInvalidSchema)
		}
	}()

	if err := s.resolved.Validate(instance); err != nil {
		return fmt.Errorf("%w: %w", ErrSchemaViolation, err)
	}

	return nil
}

// MarshalJSON implements [json.Marshaler].
func (s PortSchema) MarshalJSON() ([]byte, error) {
	if !s.IsValid() {
		return nil, ErrInvalidSchema
	}

	return s.RawJSON(), nil
}

// UnmarshalJSON implements [json.Unmarshaler].
func (s *PortSchema) UnmarshalJSON(data []byte) error {
	if s == nil {
		return fmt.Errorf("%w: nil destination", ErrInvalidSchema)
	}

	parsed, err := ParsePortSchema(data)
	if err != nil {
		return err
	}

	*s = parsed

	return nil
}
