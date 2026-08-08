//nolint:wsl_v5 // Output validation keeps bounded decoding and schema checks adjacent.
package subagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

// OutputValidator validates one final custom-Agent response against the
// immutable contract captured in its execution plan.
type OutputValidator interface {
	Validate(string) (any, error)
}

// NewOutputContract freezes one custom output contract and computes its
// durable digest. Schema is copied so callers cannot mutate a compiled plan
// through a retained YAML/JSON slice.
func NewOutputContract(
	format OutputFormat,
	name string,
	schema json.RawMessage,
) (OutputContract, error) {
	contract := OutputContract{
		Format: format,
		Name:   name,
		Schema: bytes.Clone(schema),
	}
	contract.Digest = digestOutputContract(contract)
	if err := validateOutputContract(contract); err != nil {
		return OutputContract{}, err
	}

	return contract, nil
}

// NewOutputValidator compiles a local output validator. JSON Schema
// resolution receives no external loader, so a profile can never make result
// validation fetch an external reference.
func NewOutputValidator(contract OutputContract, limits Limits) (_ OutputValidator, returnErr error) {
	if err := validateOutputContract(contract); err != nil {
		return nil, err
	}
	if err := validateLimits(limits); err != nil {
		return nil, err
	}

	switch contract.Format {
	case OutputFormatText:
		return textOutputValidator{limits: limits}, nil
	case OutputFormatJSONSchema:
		var schema jsonschema.Schema
		if err := json.Unmarshal(contract.Schema, &schema); err != nil {
			return nil, fmt.Errorf("%w: decode output schema", ErrInvalid)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve output schema", ErrInvalid)
		}

		return jsonOutputValidator{limits: limits, schema: resolved}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported custom output format", ErrInvalid)
	}
}

type textOutputValidator struct {
	limits Limits
}

func (v textOutputValidator) Validate(text string) (any, error) {
	if len(text) > v.limits.MaxResultBytes || !utf8.ValidString(text) ||
		strings.ContainsRune(text, 0) || strings.TrimSpace(text) == "" {
		return nil, ErrInvalidResult
	}

	return text, nil
}

type jsonOutputValidator struct {
	limits Limits
	schema *jsonschema.Resolved
}

func (v jsonOutputValidator) Validate(text string) (_ any, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("%w: JSON Schema validation failed", ErrInvalidResult)
		}
	}()
	if len(text) > v.limits.MaxResultBytes || !utf8.ValidString(text) {
		return nil, ErrInvalidResult
	}

	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: decode JSON output", ErrInvalidResult)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing JSON output", ErrInvalidResult)
	}
	if v.schema == nil || v.schema.Validate(value) != nil {
		return nil, ErrInvalidResult
	}

	return value, nil
}
