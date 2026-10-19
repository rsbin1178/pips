//nolint:wsl_v5 // MCP doctor fixtures keep definitions, execution, and assertions adjacent.
package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/internal/coding/cli"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMCPDoctorServer(t *testing.T, name, tool string) *httptest.Server {
	t.Helper()

	server := sdk.NewServer(&sdk.Implementation{Name: name, Version: "v1"}, nil)
	server.AddTool(&sdk.Tool{
		Name:        tool,
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{JSONResponse: true},
	)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer
}

func writeMCPDoctorDefinitions(t *testing.T, layout paths.Layout, servers string) {
	t.Helper()

	writeCLIFile(t, layout.MCPFile(), fmt.Sprintf(`{
  "schema": "pips.mcp/v1alpha2",
  "mcpServers": {%s}
}`, servers))
}

func TestMCPDoctorReportsHealthyServerAsJSON(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	server := newMCPDoctorServer(t, "local", "lookup")
	writeMCPDoctorDefinitions(t, fixture.layout, fmt.Sprintf(
		`"local": {"type": "streamable-http", "url": %q}`, server.URL,
	))

	output, err := executeWithDependencies(t, fixture.dependencies(nil), "mcp", "doctor", "--json")
	require.NoError(t, err)

	var report struct {
		Servers []struct {
			ID      string   `json:"id"`
			State   string   `json:"state"`
			Tools   []string `json:"tools"`
			Code    string   `json:"code"`
			Message string   `json:"message"`
		} `json:"servers"`
		HealthyCount int `json:"healthy_count"`
		FailingCount int `json:"failing_count"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &report))
	require.Len(t, report.Servers, 1)
	assert.Equal(t, "local", report.Servers[0].ID)
	assert.Equal(t, "connected", report.Servers[0].State)
	assert.Equal(t, []string{"local_lookup"}, report.Servers[0].Tools)
	assert.Equal(t, 1, report.HealthyCount)
	assert.Zero(t, report.FailingCount)
	assert.NotContains(t, output, server.URL)
}

func TestMCPDoctorReportsFailureAndFiltersByServer(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	healthy := newMCPDoctorServer(t, "healthy", "lookup")
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachableURL := unreachable.URL
	unreachable.Close()

	writeMCPDoctorDefinitions(t, fixture.layout, fmt.Sprintf(
		`"healthy": {"type": "streamable-http", "url": %q},
		"bad": {"type": "streamable-http", "url": %q}`, healthy.URL, unreachableURL,
	))

	output, err := executeWithDependencies(t, fixture.dependencies(nil), "mcp", "doctor", "--json")
	require.NoError(t, err)

	var report struct {
		Servers []struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Code  string `json:"code"`
		} `json:"servers"`
		HealthyCount int `json:"healthy_count"`
		FailingCount int `json:"failing_count"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &report))
	require.Len(t, report.Servers, 2)
	byID := make(map[string]struct {
		State string
		Code  string
	}, len(report.Servers))
	for _, server := range report.Servers {
		byID[server.ID] = struct {
			State string
			Code  string
		}{State: server.State, Code: server.Code}
	}
	assert.Equal(t, "connected", byID["healthy"].State)
	assert.Equal(t, "failed", byID["bad"].State)
	assert.Equal(t, "connect_failed", byID["bad"].Code)
	assert.Equal(t, 1, report.HealthyCount)
	assert.Equal(t, 1, report.FailingCount)
	assert.NotContains(t, output, unreachableURL)

	output, err = executeWithDependencies(
		t, fixture.dependencies(nil), "mcp", "doctor", "healthy", "--json",
	)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(output), &report))
	require.Len(t, report.Servers, 1)
	assert.Equal(t, "healthy", report.Servers[0].ID)

	output, err = executeWithDependencies(
		t, fixture.dependencies(nil), "mcp", "doctor", "missing", "--json",
	)
	require.ErrorIs(t, err, cli.ErrUsage)
	assert.Empty(t, output)
}

func TestMCPDoctorTextReportIsCredentialFree(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	server := newMCPDoctorServer(t, "local", "lookup")
	writeMCPDoctorDefinitions(t, fixture.layout, fmt.Sprintf(
		`"local": {"type": "streamable-http", "url": %q}`, server.URL,
	))

	output, err := executeWithDependencies(t, fixture.dependencies(nil), "mcp", "doctor")
	require.NoError(t, err)
	assert.Contains(t, output, "MCP Doctor")
	assert.Contains(t, output, "local (streamable_http)")
	assert.Contains(t, output, "ok connected")
	assert.Contains(t, output, "Found 1 healthy, 0 failing.")
	assert.NotContains(t, output, server.URL)
}
