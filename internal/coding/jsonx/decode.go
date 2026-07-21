// Package jsonx provides strict JSON decoding for Coding application stores
// and protocols.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	// ErrDuplicateKey means an object declares the same key more than once.
	ErrDuplicateKey = errors.New("coding json: duplicate object key")
	// ErrMultipleValues means input contains more than one top-level value.
	ErrMultipleValues = errors.New("coding json: multiple values")
)

// Decode decodes exactly one JSON value, rejects duplicate keys at every
// nesting level, and disallows fields unknown to target.
func Decode(data []byte, target any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return err
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrMultipleValues
		}

		return err
	}

	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanValue(decoder); err != nil {
		return err
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrMultipleValues
		}

		return err
	}

	return nil
}

//nolint:gocyclo // Recursive object/array parsing keeps duplicate detection in one pass.
func scanValue(decoder *json.Decoder) error {
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
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}

			key, keyOK := keyToken.(string)
			if !keyOK {
				return errors.New("coding json: object key is not a string")
			}

			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%w: %q", ErrDuplicateKey, key)
			}

			seen[key] = struct{}{}

			if valueErr := scanValue(decoder); valueErr != nil {
				return valueErr
			}
		}
	case '[':
		for decoder.More() {
			if valueErr := scanValue(decoder); valueErr != nil {
				return valueErr
			}
		}
	default:
		return errors.New("coding json: unexpected closing delimiter")
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
		return errors.New("coding json: mismatched delimiter")
	}

	return nil
}
