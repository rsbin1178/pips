// Package mcp loads, authorizes, and connects Coding Agent MCP integrations.
//
//nolint:wsl_v5 // Closed transport validation keeps field checks adjacent.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/execution"
	"golang.org/x/net/http/httpguts"
)

const (
	// DefinitionSchema is the current strict MCP definition file schema: a
	// portable `mcpServers` object shared with Agent Plugins mcp.json.
	DefinitionSchema = "pips.mcp/v1alpha2"
	// LegacyDefinitionSchema is the original `servers`-array schema. It is
	// still accepted with unchanged rules and fingerprints.
	LegacyDefinitionSchema = "pips.mcp/v1alpha1"
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
	ScopeUser        Scope = "user"
	ScopeProject     Scope = "project"
	ScopeSession     Scope = "session"
	ScopeAgentPlugin Scope = "agent-plugin"
)

// TransportType identifies the configured MCP transport.
type TransportType string

// Supported P0 MCP transports.
const (
	TransportStdio          TransportType = "stdio"
	TransportStreamableHTTP TransportType = "streamable_http"
)

// Visibility controls which Coding model catalog may describe one configured
// server. It is application policy, not MCP protocol metadata.
type Visibility string

const (
	// VisibilityAmbient exposes tools to the parent Coding interaction and to
	// custom children selected through the ordinary capability intersection.
	VisibilityAmbient Visibility = "ambient"
	// VisibilityAgentPrivate keeps tools out of the parent catalog. A custom
	// Agent must bind the exact server ID and fingerprint before its tools can
	// enter that child's capability intersection.
	VisibilityAgentPrivate Visibility = "agent_private"
)

// Definition is one validated MCP server declaration.
type Definition struct {
	ID             string
	Scope          Scope
	Transport      TransportType
	Visibility     Visibility
	Command        string
	Args           []string
	Environment    []execution.EnvVar
	WorkingDir     string
	PluginRoot     string
	PluginData     string
	URL            string
	Headers        []HTTPHeader
	ConnectTimeout time.Duration
}

// HTTPHeader is one validated header from an Agent Plugin or native MCP entry.
// Native values may contain ${env:NAME} credential references.
type HTTPHeader struct {
	Name  string
	Value string
}

// Fingerprint returns the normalized semantic SHA-256 identity of a Definition.
func (d Definition) Fingerprint() string {
	environment := slices.Clone(d.Environment)
	slices.SortFunc(environment, func(left, right execution.EnvVar) int {
		return strings.Compare(left.Name, right.Name)
	})

	canonical := struct {
		ID          string             `json:"id"`
		Transport   TransportType      `json:"transport"`
		Visibility  Visibility         `json:"visibility,omitempty"`
		Command     string             `json:"command,omitempty"`
		Args        []string           `json:"args,omitempty"`
		Environment []execution.EnvVar `json:"environment,omitempty"`
		WorkingDir  string             `json:"working_dir,omitempty"`
		PluginRoot  string             `json:"plugin_root,omitempty"`
		PluginData  string             `json:"plugin_data,omitempty"`
		URL         string             `json:"url,omitempty"`
		Headers     []HTTPHeader       `json:"headers,omitempty"`
		TimeoutNS   int64              `json:"connect_timeout_ns"`
	}{
		ID: d.ID, Transport: d.Transport, Visibility: d.privateVisibility(), Command: d.Command,
		Args: slices.Clone(d.Args), Environment: environment,
		WorkingDir: d.WorkingDir, PluginRoot: d.PluginRoot, PluginData: d.PluginData,
		URL: d.URL, Headers: slices.Clone(d.Headers), TimeoutNS: int64(d.ConnectTimeout),
	}

	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:])
}

func (d Definition) privateVisibility() Visibility {
	if d.effectiveVisibility() == VisibilityAgentPrivate {
		return VisibilityAgentPrivate
	}

	return ""
}

// EffectiveVisibility returns the normalized visibility used by validation,
// fingerprinting, and catalog filtering. The zero value preserves the
// pre-private-MCP ambient behavior for programmatic callers.
func (d Definition) EffectiveVisibility() Visibility { return d.effectiveVisibility() }

func (d Definition) effectiveVisibility() Visibility {
	if d.Visibility == "" {
		return VisibilityAmbient
	}

	return d.Visibility
}

func cloneDefinition(definition Definition) Definition {
	definition.Args = slices.Clone(definition.Args)
	definition.Environment = slices.Clone(definition.Environment)
	definition.Headers = slices.Clone(definition.Headers)

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
	if visibility := definition.effectiveVisibility(); visibility != VisibilityAmbient && visibility != VisibilityAgentPrivate {
		return errors.New("unsupported MCP visibility")
	}

	switch definition.Scope {
	case ScopeUser, ScopeProject, ScopeSession, ScopeAgentPlugin:
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

//nolint:gocyclo // Session, native, and portable stdio contracts share one closed validation boundary.
func validateStdioDefinition(definition Definition) error {
	if definition.URL != "" || len(definition.Headers) != 0 {
		return errors.New("stdio definition cannot set URL or headers")
	}

	if definition.Scope == ScopeSession {
		if !filepath.IsAbs(definition.Command) || filepath.Clean(definition.Command) != definition.Command ||
			!validText(definition.Command, 32<<10, false) {
			return errors.New("stdio command must be a clean absolute path")
		}
	} else if !validConfiguredCommand(definition.Command) {
		return errors.New("stdio command must be a bare executable name or a clean absolute path")
	}

	if len(definition.Args) > 256 {
		return errors.New("stdio definition has too many arguments")
	}
	switch definition.Scope {
	case ScopeAgentPlugin:
		if err := validateConfiguredEnvironment(definition.Environment, true); err != nil {
			return err
		}
		if !validAgentPluginPath(definition.PluginRoot) || !validAgentPluginPath(definition.PluginData) ||
			!validAgentPluginPath(definition.WorkingDir) {
			return errors.New("agent plugin stdio paths must be clean and absolute")
		}
	case ScopeSession:
		if err := execution.ValidateEnvironment(definition.Environment); err != nil {
			return err
		}
	default:
		if err := validateConfiguredEnvironment(definition.Environment, false); err != nil {
			return err
		}
	}
	if definition.Scope != ScopeAgentPlugin &&
		(definition.WorkingDir != "" || definition.PluginRoot != "" || definition.PluginData != "") {
		return errors.New("native stdio definition cannot set Agent Plugin paths")
	}

	total := 0

	for _, argument := range definition.Args {
		valid := len(argument) <= 32<<10 && utf8.ValidString(argument) &&
			!strings.ContainsRune(argument, '\x00')
		if definition.Scope == ScopeSession {
			valid = validText(argument, 32<<10, true)
		}
		if !valid {
			return errors.New("stdio definition has an invalid argument")
		}

		total += len(argument)
		if total > 256<<10 {
			return errors.New("stdio arguments exceed byte limit")
		}
	}

	return nil
}

// validateConfiguredEnvironment checks a trusted configuration overlay from a
// native definition file or an Agent Plugin. Values are opaque, so only
// well-formedness and bounds apply; native values must use well-formed
// ${env:NAME} credential references.
func validateConfiguredEnvironment(environment []execution.EnvVar, plugin bool) error {
	if len(environment) > 256 {
		return errors.New("stdio environment has too many entries")
	}
	total := 0
	for _, variable := range environment {
		if variable.Name == "" || strings.ContainsAny(variable.Name, "=\x00") ||
			strings.ContainsRune(variable.Value, '\x00') {
			return errors.New("stdio environment contains an invalid entry")
		}
		if plugin && (variable.Name == "PLUGIN_ROOT" || variable.Name == "PLUGIN_DATA") {
			return errors.New("agent plugin environment contains a reserved entry")
		}
		if !plugin {
			if err := validateCredentialReferences(variable.Value); err != nil {
				return fmt.Errorf("stdio environment %s: %w", variable.Name, err)
			}
		}
		total += len(variable.Name) + len(variable.Value)
		if total > 256<<10 {
			return errors.New("stdio environment exceeds byte limit")
		}
	}

	return nil
}

//nolint:gocyclo // URL, transport-field, and header constraints form one closed variant.
func validateHTTPDefinition(definition Definition) error {
	if definition.Command != "" || len(definition.Args) != 0 || len(definition.Environment) != 0 ||
		definition.WorkingDir != "" || definition.PluginRoot != "" || definition.PluginData != "" {
		return errors.New("streamable HTTP definition cannot set stdio fields")
	}

	if !validText(definition.URL, 4<<10, false) {
		return errors.New("streamable HTTP URL is invalid")
	}

	parsed, err := url.Parse(definition.URL)
	if err != nil || strings.ContainsRune(definition.URL, '#') || parsed.Host == "" ||
		parsed.User != nil || parsed.Fragment != "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) {
		return errors.New("streamable HTTP URL must be an HTTP(S) endpoint without credentials or fragment")
	}
	seenHeaders := make(map[string]struct{}, len(definition.Headers))
	for _, header := range definition.Headers {
		name := http.CanonicalHeaderKey(header.Name)
		if !httpguts.ValidHeaderFieldName(header.Name) ||
			!httpguts.ValidHeaderFieldValue(header.Value) ||
			!validText(header.Value, 32<<10, true) {
			return errors.New("streamable HTTP header is invalid")
		}
		if definition.expandsCredentials() {
			if err := validateCredentialReferences(header.Value); err != nil {
				return fmt.Errorf("streamable HTTP header %s: %w", header.Name, err)
			}
		}
		name = strings.ToLower(name)
		if _, duplicate := seenHeaders[name]; duplicate {
			return errors.New("streamable HTTP headers contain a case-insensitive duplicate")
		}
		seenHeaders[name] = struct{}{}
	}

	return nil
}

func validAgentPluginPath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value &&
		len(value) <= 32<<10 && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

// validConfiguredCommand accepts one bare executable token resolved with
// platform executable search, or a clean absolute path.
func validConfiguredCommand(value string) bool {
	if value == "" || len(value) > 32<<10 || !utf8.ValidString(value) ||
		strings.ContainsRune(value, '\x00') {
		return false
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value) == value
	}

	return !strings.ContainsAny(value, "/\\")
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
