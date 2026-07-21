// Package mcp loads, authorizes, and connects Coding Agent MCP integrations.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// DefinitionSchema is the strict MCP definition file schema.
	DefinitionSchema = "pips.mcp/v1alpha1"
	// PermissionSchema is the strict project-local permission file schema.
	PermissionSchema = "pips.permissions/v1alpha1"
)

var (
	// ErrInvalid reports malformed definitions, permissions, or options.
	ErrInvalid = errors.New("coding mcp: invalid")
	// ErrLimitExceeded reports a bounded file, entry, or scalar limit.
	ErrLimitExceeded = errors.New("coding mcp: limit exceeded")
	// ErrDuplicate reports an identity collision without precedence.
	ErrDuplicate = errors.New("coding mcp: duplicate")
	// ErrUnsafeFile reports a symlink, tracked file, broad permissions, or
	// unsupported filesystem object at a security boundary.
	ErrUnsafeFile = errors.New("coding mcp: unsafe file")
)

// Scope identifies who supplied an MCP definition.
type Scope string

// Supported MCP definition scopes.
const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// TransportType identifies the configured MCP transport.
type TransportType string

// Supported P0 MCP transports.
const (
	TransportStdio          TransportType = "stdio"
	TransportStreamableHTTP TransportType = "streamable_http"
)

// Definition is one validated MCP server declaration.
type Definition struct {
	ID             string
	Scope          Scope
	Transport      TransportType
	Command        string
	Args           []string
	URL            string
	ConnectTimeout time.Duration
}

// Fingerprint returns the normalized semantic SHA-256 identity of a Definition.
func (d Definition) Fingerprint() string {
	canonical := struct {
		ID        string        `json:"id"`
		Transport TransportType `json:"transport"`
		Command   string        `json:"command,omitempty"`
		Args      []string      `json:"args,omitempty"`
		URL       string        `json:"url,omitempty"`
		TimeoutNS int64         `json:"connect_timeout_ns"`
	}{
		ID: d.ID, Transport: d.Transport, Command: d.Command,
		Args: slices.Clone(d.Args), URL: d.URL, TimeoutNS: int64(d.ConnectTimeout),
	}

	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:])
}

func cloneDefinition(definition Definition) Definition {
	definition.Args = slices.Clone(definition.Args)

	return definition
}

var serverIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*$`)

func validateDefinition(definition Definition) error {
	if !serverIDPattern.MatchString(definition.ID) || len(definition.ID) > 48 {
		return errors.New("server ID must be 1-48 lowercase letters, digits, hyphens, or underscores")
	}

	if definition.ConnectTimeout <= 0 || definition.ConnectTimeout > time.Minute {
		return errors.New("connect timeout must be positive and at most one minute")
	}

	switch definition.Scope {
	case ScopeUser, ScopeProject:
	default:
		return errors.New("unsupported definition scope")
	}

	switch definition.Transport {
	case TransportStdio:
		return validateStdioDefinition(definition)
	case TransportStreamableHTTP:
		return validateHTTPDefinition(definition)
	default:
		return errors.New("unsupported MCP transport")
	}
}

func validateStdioDefinition(definition Definition) error {
	if definition.URL != "" {
		return errors.New("stdio definition cannot set URL")
	}

	if !filepath.IsAbs(definition.Command) || filepath.Clean(definition.Command) != definition.Command ||
		!validText(definition.Command, 32<<10, false) {
		return errors.New("stdio command must be a clean absolute path")
	}

	if len(definition.Args) > 256 {
		return errors.New("stdio definition has too many arguments")
	}

	total := 0

	for _, argument := range definition.Args {
		if !validText(argument, 32<<10, true) {
			return errors.New("stdio definition has an invalid argument")
		}

		total += len(argument)
		if total > 256<<10 {
			return errors.New("stdio arguments exceed byte limit")
		}
	}

	return nil
}

func validateHTTPDefinition(definition Definition) error {
	if definition.Command != "" || len(definition.Args) != 0 {
		return errors.New("streamable HTTP definition cannot set command or args")
	}

	if !validText(definition.URL, 4<<10, false) {
		return errors.New("streamable HTTP URL is invalid")
	}

	parsed, err := url.Parse(definition.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("streamable HTTP URL must be an HTTP(S) endpoint without credentials or fragment")
	}

	return nil
}

func validText(value string, maximum int, allowEmpty bool) bool {
	if (!allowEmpty && value == "") || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}

	for _, character := range value {
		if character == 0 || unicode.IsControl(character) && character != '\t' {
			return false
		}
	}

	return !strings.ContainsRune(value, '\r') && !strings.ContainsRune(value, '\n')
}
