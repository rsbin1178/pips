package acp

import (
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin/pips/internal/coding/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertMCPServersCreatesSessionScopedDefinitions(t *testing.T) {
	t.Parallel()

	definitions, err := convertMCPServers([]acpsdk.McpServer{{Stdio: &acpsdk.McpServerStdio{
		Name: "Go Tools", Command: "/usr/bin/gopls", Args: []string{"serve"},
		Env: []acpsdk.EnvVariable{{Name: "LANG", Value: "C.UTF-8"}},
	}}}, 4)
	require.NoError(t, err)

	values := definitions.List()
	require.Len(t, values, 1)
	assert.Equal(t, mcp.ScopeSession, values[0].Scope)
	assert.Equal(t, "go-tools-0d9c6ba37d3d", values[0].ID)
	assert.Equal(t, "/usr/bin/gopls", values[0].Command)
	assert.Equal(t, "LANG", values[0].Environment[0].Name)
}

func TestConvertMCPServersRejectsUnsupportedTransportAndRedactsEnvironment(t *testing.T) {
	t.Parallel()

	_, err := convertMCPServers([]acpsdk.McpServer{{Http: &acpsdk.McpServerHttpInline{
		Name: "remote", Type: "http", Url: "https://example.test", Headers: []acpsdk.HttpHeader{},
	}}}, 4)
	require.ErrorIs(t, err, ErrInvalid)

	secret := "must-not-leak"
	_, err = convertMCPServers([]acpsdk.McpServer{{Stdio: &acpsdk.McpServerStdio{
		Name: "unsafe", Command: "/bin/echo", Args: []string{},
		Env: []acpsdk.EnvVariable{{Name: "API_KEY", Value: secret}},
	}}}, 4)
	require.ErrorIs(t, err, ErrInvalid)
	assert.NotContains(t, err.Error(), secret)
}

func TestDeterministicServerIDHandlesUnicodeAndCollisions(t *testing.T) {
	t.Parallel()

	firstUnicodeID := deterministicServerID("服务")
	secondUnicodeID := deterministicServerID("服务")
	assert.Equal(t, firstUnicodeID, secondUnicodeID)
	assert.NotEqual(t, deterministicServerID("Go Tools"), deterministicServerID("go-tools"))
	assert.LessOrEqual(t, len(deterministicServerID(string(make([]byte, 200)))), 48)
}
