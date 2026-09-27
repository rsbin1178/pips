//nolint:wsl_v5 // Closed-variant decoding keeps each narrow failure adjacent to its field.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/execution"
	"golang.org/x/net/http/httpguts"
)

// ServerDialect selects the configuration format that owns one `mcpServers`
// entry. Both dialects share field names, closed variants, and the remote URL
// and header rules, so a server behaves the same wherever it is declared.
type ServerDialect uint8

const (
	// DialectAgentPlugin is the exact Agent Plugins 1.0.0 mcp.json entry.
	DialectAgentPlugin ServerDialect = iota + 1
	// DialectNative is a pips.mcp/v1alpha2 entry: the portable shape plus an
	// optional stdio `type`, the `http` alias, `visibility`, and
	// `connect_timeout`.
	DialectNative
)

// ErrUnsupportedTransport reports a well-formed entry whose transport pips does
// not implement, such as the legacy HTTP+SSE transport.
var ErrUnsupportedTransport = errors.New("coding mcp: unsupported transport")

// ServerEntry is one validated `mcpServers` member. Values are kept as
// written: Agent Plugin placeholders and native credential references are
// expanded by their owners, never here.
type ServerEntry struct {
	Transport TransportType
	Command   string
	Args      []string
	// Environment is sorted by name.
	Environment []execution.EnvVar
	// WorkingDir is the Agent Plugin `cwd` as written; HasWorkingDir reports
	// whether it was present.
	WorkingDir    string
	HasWorkingDir bool
	// URL is the normalized remote endpoint.
	URL string
	// Headers are sorted by name.
	Headers    []HTTPHeader
	Visibility Visibility
	// ConnectTimeout is zero when the native entry omits connect_timeout.
	ConnectTimeout time.Duration
}

// maxServerIDLength is the native server ID length limit.
const maxServerIDLength = 48

var nativeOnlyFields = []string{"visibility", "connect_timeout"}

// DecodeServerEntry validates one `mcpServers` member for dialect.
//
//nolint:gocyclo // Transport selection is one closed-variant boundary per dialect.
func DecodeServerEntry(raw json.RawMessage, dialect ServerDialect) (ServerEntry, error) {
	if dialect != DialectAgentPlugin && dialect != DialectNative {
		return ServerEntry{}, errors.New("unknown server dialect")
	}
	object, err := decodeServerObject(raw)
	if err != nil {
		return ServerEntry{}, err
	}

	transport, hasType, err := optionalString(object, "type")
	if err != nil {
		return ServerEntry{}, errors.New("server type must be a string")
	}
	if !hasType {
		switch {
		case dialect == DialectAgentPlugin:
			return ServerEntry{}, errors.New("server type is required")
		case object["command"] != nil:
			transport = "stdio"
		case object["url"] != nil:
			return ServerEntry{}, errors.New(`remote server needs "type": "streamable-http"`)
		default:
			return ServerEntry{}, errors.New("server type is required")
		}
	}
	if dialect == DialectNative && transport == "http" {
		transport = "streamable-http"
	}

	var entry ServerEntry
	switch transport {
	case "stdio":
		entry, err = decodeStdioEntry(object, dialect)
	case "streamable-http":
		entry, err = decodeRemoteEntry(object, dialect)
	case "sse":
		if _, err := decodeRemoteEntry(object, dialect); err != nil {
			return ServerEntry{}, err
		}

		return ServerEntry{}, fmt.Errorf("%w: legacy HTTP+SSE is not supported", ErrUnsupportedTransport)
	default:
		return ServerEntry{}, errors.New("unsupported server type")
	}
	if err != nil {
		return ServerEntry{}, err
	}
	if dialect == DialectNative {
		if err := decodeNativeFields(object, &entry); err != nil {
			return ServerEntry{}, err
		}
	}

	return entry, nil
}

//nolint:gocyclo // The stdio variant validates each closed field in declaration order.
func decodeStdioEntry(object map[string]json.RawMessage, dialect ServerDialect) (ServerEntry, error) {
	allowed := []string{"type", "command", "args", "env"}
	if dialect == DialectAgentPlugin {
		allowed = append(allowed, "cwd")
	} else {
		allowed = append(allowed, nativeOnlyFields...)
	}
	if err := rejectUnknownFields(object, allowed...); err != nil {
		return ServerEntry{}, err
	}

	command, _, err := optionalString(object, "command")
	if err != nil || command == "" {
		return ServerEntry{}, errors.New("stdio command must be a non-empty string")
	}
	if dialect == DialectNative && !validConfiguredCommand(command) {
		return ServerEntry{}, errors.New("stdio command must be a bare executable name or a clean absolute path")
	}

	var args []string
	if raw, exists := object["args"]; exists {
		if err := json.Unmarshal(raw, &args); err != nil || args == nil {
			return ServerEntry{}, errors.New("stdio args must be an array of strings")
		}
	}

	environment, err := decodeStringMap(object["env"])
	if err != nil {
		return ServerEntry{}, errors.New("stdio env must be an object of strings")
	}
	variables := make([]execution.EnvVar, 0, len(environment))
	for _, name := range sortedKeys(environment) {
		if dialect == DialectAgentPlugin && (name == "PLUGIN_ROOT" || name == "PLUGIN_DATA") {
			return ServerEntry{}, fmt.Errorf("stdio env cannot override %s", name)
		}
		variables = append(variables, execution.EnvVar{Name: name, Value: environment[name]})
	}

	entry := ServerEntry{
		Transport: TransportStdio, Command: command, Args: args,
		Environment: variables,
	}
	if raw, exists := object["cwd"]; exists {
		if err := json.Unmarshal(raw, &entry.WorkingDir); err != nil {
			return ServerEntry{}, errors.New("stdio cwd must be a string")
		}
		entry.HasWorkingDir = true
	}

	return entry, nil
}

func decodeRemoteEntry(object map[string]json.RawMessage, dialect ServerDialect) (ServerEntry, error) {
	allowed := []string{"type", "url", "headers"}
	if dialect == DialectNative {
		allowed = append(allowed, nativeOnlyFields...)
	}
	if err := rejectUnknownFields(object, allowed...); err != nil {
		return ServerEntry{}, err
	}

	endpoint, _, err := optionalString(object, "url")
	if err != nil || endpoint == "" {
		return ServerEntry{}, errors.New("remote URL must be a non-empty string")
	}
	endpoint, err = normalizeRemoteURL(endpoint)
	if err != nil {
		return ServerEntry{}, err
	}
	headers, err := decodeHeaders(object["headers"])
	if err != nil {
		return ServerEntry{}, err
	}

	return ServerEntry{Transport: TransportStreamableHTTP, URL: endpoint, Headers: headers}, nil
}

func decodeNativeFields(object map[string]json.RawMessage, entry *ServerEntry) error {
	visibility, _, err := optionalString(object, "visibility")
	if err != nil {
		return errors.New("visibility must be a string")
	}
	entry.Visibility = Visibility(visibility)

	timeout, hasTimeout, err := optionalString(object, "connect_timeout")
	if err != nil {
		return errors.New("connect_timeout must be a duration string")
	}
	if hasTimeout {
		parsed, err := time.ParseDuration(timeout)
		if err != nil {
			return errors.New("connect_timeout must be a duration such as 30s")
		}
		// Zero means "omitted" to the caller, so an explicit zero must fail
		// here rather than silently selecting the default.
		if parsed <= 0 {
			return errors.New("connect timeout must be positive and at most one minute")
		}
		entry.ConnectTimeout = parsed
	}

	return nil
}

// normalizeRemoteURL applies the Agent Plugins remote endpoint rule: an
// absolute HTTP(S) URL without userinfo or fragment, using HTTPS unless the
// host is exactly localhost or a loopback IP literal.
func normalizeRemoteURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || !validRemoteURLSyntax(value, parsed) {
		return "", errors.New("remote URL must be an absolute HTTP(S) URL without credentials or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme == "https" {
		return parsed.String(), nil
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return parsed.String(), nil
	}
	address := net.ParseIP(host)
	if address == nil || !address.IsLoopback() {
		return "", errors.New("remote URL must use HTTPS unless it targets localhost or a loopback address")
	}

	return parsed.String(), nil
}

func validRemoteURLSyntax(value string, parsed *url.URL) bool {
	return !strings.ContainsRune(value, '#') && parsed.IsAbs() && parsed.Host != "" &&
		parsed.Opaque == "" && parsed.User == nil && parsed.Fragment == "" &&
		(strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
}

func decodeHeaders(raw json.RawMessage) ([]HTTPHeader, error) {
	values, err := decodeStringMap(raw)
	if err != nil {
		return nil, errors.New("headers must be an object of strings")
	}
	seen := make(map[string]struct{}, len(values))
	headers := make([]HTTPHeader, 0, len(values))
	for _, name := range sortedKeys(values) {
		canonical := strings.ToLower(http.CanonicalHeaderKey(name))
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(values[name]) {
			return nil, errors.New("header name or value is invalid")
		}
		if _, duplicate := seen[canonical]; duplicate {
			return nil, errors.New("headers contain a case-insensitive duplicate")
		}
		seen[canonical] = struct{}{}
		headers = append(headers, HTTPHeader{Name: name, Value: values[name]})
	}

	return headers, nil
}

// ServerID derives the native server ID from an `mcpServers` name. The name is
// lowercased and every run of characters outside [a-z0-9_] becomes one hyphen
// so the ID satisfies the native pattern; names that would exceed the length
// limit are truncated and suffixed with a short content hash so two long names
// stay distinct.
func ServerID(name string) string {
	var builder strings.Builder
	separator := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			separator = false
		case r == '_' && !separator && builder.Len() > 0:
			builder.WriteRune(r)
			separator = true
		case !separator && builder.Len() > 0:
			builder.WriteRune('-')
			separator = true
		}
	}
	id := strings.TrimRight(builder.String(), "-_")
	if id == "" {
		id = "server"
	}
	if len(id) <= maxServerIDLength {
		return id
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:4])
	id = strings.TrimRight(id[:maxServerIDLength-len(suffix)-1], "-_")

	return id + "-" + suffix
}

func decodeServerObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if !utf8.Valid(raw) || json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, errors.New("server must be an object")
	}

	return object, nil
}

func optionalString(object map[string]json.RawMessage, field string) (string, bool, error) {
	raw, exists := object[field]
	if !exists {
		return "", false, nil
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return "", true, fmt.Errorf("%s must be a string", field)
	}

	return *value, true, nil
}

func decodeStringMap(raw json.RawMessage) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, errors.New("expected an object of strings")
	}

	return values, nil
}

func rejectUnknownFields(object map[string]json.RawMessage, allowed ...string) error {
	for _, field := range sortedKeys(object) {
		if !slices.Contains(allowed, field) {
			return fmt.Errorf("unknown server field %q", field)
		}
	}

	return nil
}

func sortedKeys[Value any](values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}
