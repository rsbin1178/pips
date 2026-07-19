package agentmcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildConfigValidatesOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options []Option
		wantErr string
	}{
		{name: "nil option", options: []Option{nil}, wantErr: "nil option"},
		{name: "empty prefix", options: []Option{WithToolNamePrefix("")}, wantErr: "empty tool name prefix"},
		{name: "invalid prefix", options: []Option{WithToolNamePrefix("bad.prefix")}, wantErr: "unsupported character"},
		{name: "long prefix", options: []Option{WithToolNamePrefix(strings.Repeat("a", 63))}, wantErr: "exceeds"},
		{name: "nil mapper", options: []Option{WithToolNameMapper(nil)}, wantErr: "nil tool name mapper"},
		{name: "zero max", options: []Option{WithMaxTools(0)}, wantErr: "must be positive"},
		{
			name: "conflicting sampling handlers",
			options: []Option{WithClientOptions(&mcp.ClientOptions{
				CreateMessageHandler: func(_ context.Context, _ *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
					return nil, nil
				},
				CreateMessageWithToolsHandler: func(_ context.Context, _ *mcp.CreateMessageWithToolsRequest) (*mcp.CreateMessageWithToolsResult, error) {
					return nil, nil
				},
			})},
			wantErr: "both sampling handlers",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := buildConfig(test.options)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestToolNameMappingAndShortening(t *testing.T) {
	t.Parallel()

	name, err := defaultToolName("files.read/中文")
	require.NoError(t, err)
	assert.Equal(t, "files_read___", name)

	longName := strings.Repeat("a", 80)
	shortened := shortenToolName(longName)
	assert.Len(t, shortened, maxToolNameLen)
	assert.Equal(t, shortened, shortenToolName(longName))
	assert.NotEqual(t, shortened, shortenToolName(strings.Repeat("b", 80)))

	want := errors.New("mapping failed")
	client := newFakeClient(t, &fakeToolSession{tools: []*mcp.Tool{rawTool("remote")}},
		WithToolNameMapper(func(string) (string, error) {
			return "", want
		}),
	)
	_, err = client.Tools(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}
