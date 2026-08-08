//nolint:wsl_v5 // Closed-schema validation keeps each field decision adjacent.
package agentplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"unicode/utf8"
)

var (
	pluginNamePattern       = regexp.MustCompile(`^(?:[a-z0-9]|[a-z0-9](?:[a-z0-9.-]*[a-z0-9]))$`)
	pluginNameRepeatPattern = regexp.MustCompile(`--|\.\.`)
)

var manifestFields = map[string]struct{}{
	"$schema": {}, "name": {}, "version": {}, "description": {}, "author": {},
	"homepage": {}, "repository": {}, "license": {}, "keywords": {}, "extensions": {},
}

//nolint:gocyclo // Closed-schema field validation stays explicit and auditable.
func decodeManifest(data []byte) (Manifest, []Diagnostic, error) {
	object, err := decodeObject(data)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("%w: plugin.json must be a JSON object: %w", ErrInvalid, err)
	}

	var diagnostics []Diagnostic
	for field := range object {
		if _, known := manifestFields[field]; !known {
			diagnostics = append(diagnostics, Diagnostic{
				Component: "manifest", Code: "unknown_field_ignored",
				Message: fmt.Sprintf("unknown top-level field %q was ignored", field),
			})
		}
	}

	schema, err := requiredString(object, "$schema")
	if err != nil || schema != ManifestSchema {
		return Manifest{}, diagnostics, fmt.Errorf("%w: unsupported or missing $schema", ErrInvalid)
	}
	name, err := requiredString(object, "name")
	if err != nil || !validPluginName(name) {
		return Manifest{}, diagnostics, fmt.Errorf("%w: invalid plugin name", ErrInvalid)
	}

	manifest := Manifest{Name: name}
	for field, target := range map[string]*string{
		"version": &manifest.Version, "description": &manifest.Description,
		"homepage": &manifest.Homepage, "repository": &manifest.Repository,
		"license": &manifest.License,
	} {
		if err := optionalString(object, field, target); err != nil {
			return Manifest{}, diagnostics, fmt.Errorf("%w: %s must be a string", ErrInvalid, field)
		}
	}

	if raw, exists := object["author"]; exists {
		author, authorErr := decodeAuthor(raw)
		if authorErr != nil {
			return Manifest{}, diagnostics, authorErr
		}
		manifest.Author = author
	}
	if raw, exists := object["keywords"]; exists {
		if err := json.Unmarshal(raw, &manifest.Keywords); err != nil || manifest.Keywords == nil {
			return Manifest{}, diagnostics, fmt.Errorf("%w: keywords must be an array of strings", ErrInvalid)
		}
	}
	if raw, exists := object["extensions"]; exists {
		var extensionObject map[string]json.RawMessage
		if err := json.Unmarshal(raw, &extensionObject); err != nil || extensionObject == nil {
			diagnostics = append(diagnostics, Diagnostic{
				Component: "manifest", Code: "extensions_ignored",
				Message: "non-object extensions field was ignored",
			})
		}
	}

	slices.SortFunc(diagnostics, compareDiagnostic)

	return manifest, diagnostics, nil
}

func decodeAuthor(raw json.RawMessage) (*Author, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: author must be an object", ErrInvalid)
	}
	for field := range object {
		if field != "name" && field != "email" && field != "url" {
			return nil, fmt.Errorf("%w: unknown author field %q", ErrInvalid, field)
		}
	}

	author := &Author{}
	for field, target := range map[string]*string{
		"name": &author.Name, "email": &author.Email, "url": &author.URL,
	} {
		if err := optionalString(object, field, target); err != nil {
			return nil, fmt.Errorf("%w: author.%s must be a string", ErrInvalid, field)
		}
	}

	return author, nil
}

func validPluginName(name string) bool {
	return len(name) <= 64 && pluginNamePattern.MatchString(name) &&
		!pluginNameRepeatPattern.MatchString(name)
}

func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("expected UTF-8 JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("expected object")
	}

	return object, nil
}

func requiredString(object map[string]json.RawMessage, field string) (string, error) {
	raw, exists := object[field]
	if !exists {
		return "", fmt.Errorf("missing %s", field)
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	if value == nil {
		return "", errors.New("expected string")
	}

	return *value, nil
}

func optionalString(object map[string]json.RawMessage, field string, target *string) error {
	raw, exists := object[field]
	if !exists {
		return nil
	}

	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value == nil {
		return errors.New("expected string")
	}
	*target = *value

	return nil
}

func compareDiagnostic(left, right Diagnostic) int {
	if value := compareString(left.Plugin, right.Plugin); value != 0 {
		return value
	}
	if value := compareString(left.Component, right.Component); value != 0 {
		return value
	}
	if value := compareString(left.Code, right.Code); value != 0 {
		return value
	}

	return compareString(left.Message, right.Message)
}

func compareString(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
