//nolint:wsl_v5 // Dialect tables keep each input adjacent to its assertions.
package mcp_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/execution"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeServerEntrySharesPortableVariantsAcrossDialects(t *testing.T) {
	t.Parallel()

	for _, dialect := range []codingmcp.ServerDialect{codingmcp.DialectAgentPlugin, codingmcp.DialectNative} {
		stdio, err := codingmcp.DecodeServerEntry(json.RawMessage(`{
          "type":"stdio","command":"npx","args":["-y","server"],
          "env":{"B":"2","A_API_KEY":"1"}
        }`), dialect)
		require.NoError(t, err)
		assert.Equal(t, codingmcp.TransportStdio, stdio.Transport)
		assert.Equal(t, "npx", stdio.Command)
		assert.Equal(t, []string{"-y", "server"}, stdio.Args)
		assert.Equal(t, []execution.EnvVar{{Name: "A_API_KEY", Value: "1"}, {Name: "B", Value: "2"}}, stdio.Environment)

		remote, err := codingmcp.DecodeServerEntry(json.RawMessage(`{
          "type":"streamable-http","url":"HTTPS://mcp.example.test/mcp",
          "headers":{"X-Tenant":"public","Authorization":"Bearer token"}
        }`), dialect)
		require.NoError(t, err)
		assert.Equal(t, codingmcp.TransportStreamableHTTP, remote.Transport)
		assert.Equal(t, "https://mcp.example.test/mcp", remote.URL)
		assert.Equal(t, []codingmcp.HTTPHeader{
			{Name: "Authorization", Value: "Bearer token"}, {Name: "X-Tenant", Value: "public"},
		}, remote.Headers)

		_, err = codingmcp.DecodeServerEntry(json.RawMessage(`{"type":"sse","url":"https://legacy.example.test/sse"}`), dialect)
		require.ErrorIs(t, err, codingmcp.ErrUnsupportedTransport)
	}
}

func TestDecodeServerEntryDialectDifferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		entry  string
		plugin bool
		native bool
	}{
		{name: "omitted stdio type", entry: `{"command":"uvx"}`, native: true},
		{name: "omitted remote type", entry: `{"url":"https://example.test/mcp"}`},
		{name: "http alias", entry: `{"type":"http","url":"https://example.test/mcp"}`, native: true},
		{name: "legacy native type name", entry: `{"type":"streamable_http","url":"https://example.test/mcp"}`},
		{name: "plugin cwd", entry: `{"type":"stdio","command":"uvx","cwd":"./data"}`, plugin: true},
		{
			name:   "native fields",
			entry:  `{"type":"stdio","command":"uvx","visibility":"agent_private","connect_timeout":"30s"}`,
			native: true,
		},
		{name: "plugin-relative command", entry: `{"type":"stdio","command":"./bin/server"}`, plugin: true},
		{name: "absolute command", entry: `{"type":"stdio","command":"/usr/bin/env"}`, plugin: true, native: true},
		{name: "reserved plugin env", entry: `{"type":"stdio","command":"uvx","env":{"PLUGIN_ROOT":"x"}}`, native: true},
		{name: "remote with stdio field", entry: `{"type":"streamable-http","url":"https://example.test","command":"x"}`},
		{name: "unknown field", entry: `{"type":"stdio","command":"uvx","enabled":true}`},
		{name: "non-loopback HTTP", entry: `{"type":"streamable-http","url":"http://example.test/mcp"}`},
		{name: "loopback HTTP", entry: `{"type":"streamable-http","url":"http://127.0.0.1:9/mcp"}`, plugin: true, native: true},
		{name: "userinfo", entry: `{"type":"streamable-http","url":"https://user@example.test/mcp"}`},
		{name: "empty fragment", entry: `{"type":"streamable-http","url":"https://example.test/mcp#"}`},
		{
			name:  "case-insensitive duplicate header",
			entry: `{"type":"streamable-http","url":"https://example.test","headers":{"X-Key":"a","x-key":"b"}}`,
		},
		{name: "bad timeout", entry: `{"type":"stdio","command":"uvx","connect_timeout":"soon"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, pluginErr := codingmcp.DecodeServerEntry(json.RawMessage(test.entry), codingmcp.DialectAgentPlugin)
			_, nativeErr := codingmcp.DecodeServerEntry(json.RawMessage(test.entry), codingmcp.DialectNative)
			assert.Equal(t, test.plugin, pluginErr == nil, "plugin dialect: %v", pluginErr)
			assert.Equal(t, test.native, nativeErr == nil, "native dialect: %v", nativeErr)
		})
	}
}

func TestServerIDDerivesPortableNativeIDs(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "context7", codingmcp.ServerID("context7"))
	assert.Equal(t, "my-docs", codingmcp.ServerID("My Docs"))
	assert.Equal(t, "context-7", codingmcp.ServerID("Context.7"))
	assert.Equal(t, "server", codingmcp.ServerID("!!!"))

	long := strings.Repeat("a", 60)
	id := codingmcp.ServerID(long)
	assert.LessOrEqual(t, len(id), 48)
	assert.NotEqual(t, id, codingmcp.ServerID(long+"b"))
}

func TestNativeDefinitionsValidateCredentialReferences(t *testing.T) {
	t.Parallel()

	remote := func(value string) codingmcp.Definition {
		return codingmcp.Definition{
			ID: "docs", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStreamableHTTP,
			URL: "https://example.test/mcp", Headers: []codingmcp.HTTPHeader{{Name: "Authorization", Value: value}},
			ConnectTimeout: time.Second,
		}
	}
	for _, valid := range []string{"Bearer ${env:DOCS_TOKEN}", "${env:_A1}${env:B}", "literal ${HOME} and $env:X"} {
		_, err := codingmcp.NewDefinitions([]codingmcp.Definition{remote(valid)}, 1)
		require.NoError(t, err, valid)
	}
	for _, invalid := range []string{"${env:}", "${env:1BAD}", "${env:BAD-NAME}", "${env:OPEN"} {
		_, err := codingmcp.NewDefinitions([]codingmcp.Definition{remote(invalid)}, 1)
		require.ErrorIs(t, err, codingmcp.ErrInvalid, invalid)
	}

	plugin := remote("${env:}")
	plugin.Scope = codingmcp.ScopeAgentPlugin
	_, err := codingmcp.NewDefinitions([]codingmcp.Definition{plugin}, 1)
	require.NoError(t, err, "Agent Plugin values are literal")

	stdio := codingmcp.Definition{
		ID: "tools", Scope: codingmcp.ScopeProject, Transport: codingmcp.TransportStdio,
		Command:        "npx",
		Environment:    []execution.EnvVar{{Name: "GITHUB_PERSONAL_ACCESS_TOKEN", Value: "${env:GITHUB_TOKEN}"}},
		ConnectTimeout: time.Second,
	}
	_, err = codingmcp.NewDefinitions([]codingmcp.Definition{stdio}, 1)
	require.NoError(t, err, "configured native env accepts credential-style names and bare commands")

	session := stdio
	session.Scope = codingmcp.ScopeSession
	_, err = codingmcp.NewDefinitions([]codingmcp.Definition{session}, 1)
	require.ErrorIs(t, err, codingmcp.ErrInvalid, "session definitions stay strict")
}
