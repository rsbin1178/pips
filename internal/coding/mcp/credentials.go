//nolint:wsl_v5 // Template scanning keeps each cursor step adjacent to its check.
package mcp

import (
	"errors"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/execution"
	"golang.org/x/net/http/httpguts"
)

// credentialReferencePrefix opens a native credential reference. The complete
// form is ${env:NAME}, where NAME is a portable environment variable name.
const credentialReferencePrefix = "${env:"

// credentialError reports an unusable credential reference by variable name.
// It never carries the variable's value.
type credentialError struct {
	name   string
	reason string
}

func (e credentialError) Error() string {
	return "environment variable " + e.name + " referenced by this server " + e.reason
}

// expandsCredentials reports whether a definition's env and header values may
// contain ${env:NAME} references. Only native definition files use them:
// Agent Plugins forbids environment expansion and ACP supplies literal values.
func (d Definition) expandsCredentials() bool {
	return d.Scope == ScopeUser || d.Scope == ScopeProject
}

// hasCredentialReferences reports whether any env or header value references
// the Runtime environment.
func (d Definition) hasCredentialReferences() bool {
	if !d.expandsCredentials() {
		return false
	}
	for _, variable := range d.Environment {
		if strings.Contains(variable.Value, credentialReferencePrefix) {
			return true
		}
	}
	for _, header := range d.Headers {
		if strings.Contains(header.Value, credentialReferencePrefix) {
			return true
		}
	}

	return false
}

// validateCredentialReferences checks every reference in value without
// consulting the environment.
func validateCredentialReferences(value string) error {
	_, err := expandCredentialReferences(value, func(string) (string, bool) { return "x", true })

	return err
}

// expandCredentialReferences replaces every ${env:NAME} in value in one
// non-recursive pass. Text introduced by a replacement is never rescanned, and
// other ${...} text stays literal. An unset or empty variable is an error.
func expandCredentialReferences(value string, lookup func(string) (string, bool)) (string, error) {
	var expanded strings.Builder
	rest := value
	for {
		start := strings.Index(rest, credentialReferencePrefix)
		if start < 0 {
			expanded.WriteString(rest)

			return expanded.String(), nil
		}
		expanded.WriteString(rest[:start])
		rest = rest[start+len(credentialReferencePrefix):]

		end := strings.IndexByte(rest, '}')
		if end < 0 || !validReferenceName(rest[:end]) {
			return "", errors.New("credential reference must have the form ${env:NAME}")
		}
		name := rest[:end]
		rest = rest[end+1:]

		resolved, ok := "", false
		if lookup != nil {
			resolved, ok = lookup(name)
		}
		if !ok || resolved == "" {
			return "", credentialError{name: name, reason: "is not set"}
		}
		expanded.WriteString(resolved)
	}
}

func validReferenceName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		letter := character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z'
		if !letter && (index == 0 || character < '0' || character > '9') {
			return false
		}
	}

	return true
}

// resolveEnvironment returns the stdio environment overlay with credential
// references expanded from lookup.
func resolveEnvironment(
	definition Definition,
	lookup func(string) (string, bool),
) ([]execution.EnvVar, error) {
	if !definition.expandsCredentials() {
		return definition.Environment, nil
	}
	resolved := make([]execution.EnvVar, len(definition.Environment))
	for index, variable := range definition.Environment {
		value, err := expandCredentialReferences(variable.Value, lookup)
		if err != nil {
			return nil, err
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, credentialError{name: referenceNames(variable.Value), reason: "contains NUL"}
		}
		resolved[index] = execution.EnvVar{Name: variable.Name, Value: value}
	}

	return resolved, nil
}

// resolveHeaders returns the configured headers with credential references
// expanded from lookup.
func resolveHeaders(definition Definition, lookup func(string) (string, bool)) ([]HTTPHeader, error) {
	if !definition.expandsCredentials() {
		return definition.Headers, nil
	}
	resolved := make([]HTTPHeader, len(definition.Headers))
	for index, header := range definition.Headers {
		value, err := expandCredentialReferences(header.Value, lookup)
		if err != nil {
			return nil, err
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, credentialError{
				name: referenceNames(header.Value), reason: "is not a valid HTTP header value",
			}
		}
		resolved[index] = HTTPHeader{Name: header.Name, Value: value}
	}

	return resolved, nil
}

// referenceNames lists the variable names referenced by a validated template
// for diagnostics.
func referenceNames(template string) string {
	names := make([]string, 0, 1)
	rest := template
	for {
		start := strings.Index(rest, credentialReferencePrefix)
		if start < 0 {
			return strings.Join(names, ", ")
		}
		rest = rest[start+len(credentialReferencePrefix):]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			return strings.Join(names, ", ")
		}
		names = append(names, rest[:end])
		rest = rest[end+1:]
	}
}
