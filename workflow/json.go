package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	defaultMaxJSONBytes = 1 << 20
	defaultMaxJSONDepth = 64
	defaultMaxJSONItems = 100_000
)

type jsonLimits struct {
	maxBytes int
	maxDepth int
	maxItems int
}

var defaultJSONLimits = jsonLimits{
	maxBytes: defaultMaxJSONBytes,
	maxDepth: defaultMaxJSONDepth,
	maxItems: defaultMaxJSONItems,
}

func decodeJSON(data []byte, destination any, limits jsonLimits, strict bool) error {
	if len(data) == 0 || len(data) > limits.maxBytes || !utf8.Valid(data) {
		return errors.New("invalid JSON size or encoding")
	}

	if err := validateJSONStructure(data, limits); err != nil {
		return err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	if strict {
		decoder.DisallowUnknownFields()
	}

	if err := decoder.Decode(destination); err != nil {
		return err
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}

		return err
	}

	return nil
}

func validateJSONStructure(data []byte, limits jsonLimits) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	items := 0
	if err := scanJSONValue(decoder, 0, &items, limits); err != nil {
		return err
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}

		return err
	}

	return nil
}

func scanJSONValue(
	decoder *json.Decoder,
	depth int,
	items *int,
	limits jsonLimits,
) error {
	if depth > limits.maxDepth {
		return fmt.Errorf("JSON nesting exceeds %d", limits.maxDepth)
	}

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
		return scanJSONObject(decoder, depth, items, limits)
	case '[':
		return scanJSONArray(decoder, depth, items, limits)
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func scanJSONObject(
	decoder *json.Decoder,
	depth int,
	items *int,
	limits jsonLimits,
) error {
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

		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate JSON object key %q", key)
		}

		seen[key] = struct{}{}

		if err := incrementJSONItems(items, limits.maxItems); err != nil {
			return err
		}

		if err := scanJSONValue(decoder, depth+1, items, limits); err != nil {
			return err
		}
	}

	_, err := decoder.Token()

	return err
}

func scanJSONArray(
	decoder *json.Decoder,
	depth int,
	items *int,
	limits jsonLimits,
) error {
	for decoder.More() {
		if err := incrementJSONItems(items, limits.maxItems); err != nil {
			return err
		}

		if err := scanJSONValue(decoder, depth+1, items, limits); err != nil {
			return err
		}
	}

	_, err := decoder.Token()

	return err
}

func incrementJSONItems(items *int, maximum int) error {
	(*items)++
	if *items > maximum {
		return fmt.Errorf("JSON items exceed %d", maximum)
	}

	return nil
}
