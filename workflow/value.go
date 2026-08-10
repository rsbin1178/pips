package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
)

// ValueKind identifies one JSON-compatible [Value] variant.
type ValueKind uint8

// Value kinds. The zero value is invalid rather than JSON null.
const (
	ValueInvalid ValueKind = iota
	ValueNull
	ValueBool
	ValueNumber
	ValueString
	ValueArray
	ValueObject
)

// Value is an immutable, canonical JSON value. Its zero value is invalid;
// construct values with [ParseValue] or [ValueOf].
type Value struct {
	raw []byte
}

// ParseValue validates and snapshots one JSON value.
func ParseValue(data []byte) (Value, error) {
	var decoded any
	if err := decodeJSON(data, &decoded, defaultJSONLimits, false); err != nil {
		return Value{}, fmt.Errorf("%w: %w", ErrInvalidValue, err)
	}

	canonical, err := json.Marshal(decoded)
	if err != nil {
		return Value{}, fmt.Errorf("%w: encode canonical JSON: %w", ErrInvalidValue, err)
	}

	return Value{raw: canonical}, nil
}

// ValueOf converts a JSON-encodable Go value into an immutable Value.
func ValueOf(value any) (Value, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Value{}, fmt.Errorf("%w: encode %T: %w", ErrInvalidValue, value, err)
	}

	return ParseValue(data)
}

// MustValueOf is like [ValueOf] but panics when value is not JSON-compatible.
// It is intended for package-level declarations and tests.
func MustValueOf(value any) Value {
	result, err := ValueOf(value)
	if err != nil {
		panic(err)
	}

	return result
}

// Kind reports the JSON variant represented by v.
func (v Value) Kind() ValueKind {
	if len(v.raw) == 0 {
		return ValueInvalid
	}

	switch v.raw[0] {
	case 'n':
		return ValueNull
	case 't', 'f':
		return ValueBool
	case '"':
		return ValueString
	case '[':
		return ValueArray
	case '{':
		return ValueObject
	default:
		return ValueNumber
	}
}

// IsValid reports whether v contains a parsed JSON value.
func (v Value) IsValid() bool {
	return v.Kind() != ValueInvalid
}

// RawJSON returns an independent copy of v's canonical JSON representation.
func (v Value) RawJSON() []byte {
	return slices.Clone(v.raw)
}

// Any returns an independent standard-library representation of v.
func (v Value) Any() (any, error) {
	if !v.IsValid() {
		return nil, ErrInvalidValue
	}

	var decoded any
	if err := decodeJSON(v.raw, &decoded, defaultJSONLimits, false); err != nil {
		return nil, fmt.Errorf("%w: decode snapshot: %w", ErrInvalidValue, err)
	}

	return decoded, nil
}

// Lookup resolves an object key or array index path without exposing mutable
// storage. It returns false when any path segment is absent or incompatible.
func (v Value) Lookup(path ...string) (Value, bool) {
	current, err := v.Any()
	if err != nil {
		return Value{}, false
	}

	for _, segment := range path {
		switch typed := current.(type) {
		case map[string]any:
			var ok bool

			current, ok = typed[segment]
			if !ok {
				return Value{}, false
			}
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return Value{}, false
			}

			current = typed[index]
		default:
			return Value{}, false
		}
	}

	result, err := ValueOf(current)
	if err != nil {
		return Value{}, false
	}

	return result, true
}

// Equal reports equality of the canonical, number-preserving JSON snapshots.
// Object key order does not affect the result.
func (v Value) Equal(other Value) bool {
	return bytes.Equal(v.raw, other.raw)
}

// DecodeValue decodes v into T using encoding/json rules.
func DecodeValue[T any](v Value) (T, error) {
	var result T
	if !v.IsValid() {
		return result, ErrInvalidValue
	}

	if err := json.Unmarshal(v.raw, &result); err != nil {
		return result, fmt.Errorf("%w: decode into %T: %w", ErrInvalidValue, result, err)
	}

	return result, nil
}

// MarshalJSON implements [json.Marshaler].
func (v Value) MarshalJSON() ([]byte, error) {
	if !v.IsValid() {
		return nil, ErrInvalidValue
	}

	return v.RawJSON(), nil
}

// UnmarshalJSON implements [json.Unmarshaler].
func (v *Value) UnmarshalJSON(data []byte) error {
	if v == nil {
		return fmt.Errorf("%w: nil destination", ErrInvalidValue)
	}

	parsed, err := ParseValue(data)
	if err != nil {
		return err
	}

	*v = parsed

	return nil
}

// String returns v's canonical JSON representation. Invalid values render as
// an empty string.
func (v Value) String() string {
	return string(v.raw)
}
