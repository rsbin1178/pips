package agentmcp

import (
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMaxTools = 256
	maxToolNameLen  = 64
)

// ToolNameMapper maps an MCP server's tool name to the name advertised to the
// language model. The result must contain only ASCII letters, digits,
// underscores, or dashes. Names longer than 64 bytes are shortened
// deterministically after mapping and prefixing.
type ToolNameMapper func(remoteName string) (string, error)

// Option configures Connect.
type Option func(*config) error

type config struct {
	clientOptions mcp.ClientOptions
	maxTools      int
	nameMapper    ToolNameMapper
	namePrefix    string
}

func defaultConfig() config {
	return config{
		maxTools:   defaultMaxTools,
		nameMapper: defaultToolName,
	}
}

// WithClientOptions supplies official SDK client capabilities and handlers.
// The value is shallow-copied. Do not mutate referenced capability maps after
// calling Connect. Progress and tool-list handlers are chained after the
// bridge's internal handlers.
func WithClientOptions(options *mcp.ClientOptions) Option {
	return func(cfg *config) error {
		if options == nil {
			return errors.New("agent/mcp: nil client options")
		}

		cfg.clientOptions = *options

		return nil
	}
}

// WithToolNamePrefix prefixes every advertised tool name. It is useful when
// combining tools from multiple MCP servers. Prefix must use the portable tool
// name character set and leave room for a separator and tool name.
func WithToolNamePrefix(prefix string) Option {
	return func(cfg *config) error {
		if prefix == "" {
			return errors.New("agent/mcp: empty tool name prefix")
		}

		if err := validatePortableName(prefix, maxToolNameLen-2); err != nil {
			return fmt.Errorf("agent/mcp: tool name prefix: %w", err)
		}

		cfg.namePrefix = prefix

		return nil
	}
}

// WithToolNameMapper replaces the default portable-name mapping. The bridge
// still validates, prefixes, shortens, and collision-checks mapped names.
func WithToolNameMapper(mapper ToolNameMapper) Option {
	return func(cfg *config) error {
		if mapper == nil {
			return errors.New("agent/mcp: nil tool name mapper")
		}

		cfg.nameMapper = mapper

		return nil
	}
}

// WithMaxTools bounds the number of tools loaded from one server snapshot.
// The default is 256.
func WithMaxTools(maxTools int) Option {
	return func(cfg *config) error {
		if maxTools <= 0 {
			return fmt.Errorf("agent/mcp: max tools must be positive: %d", maxTools)
		}

		cfg.maxTools = maxTools

		return nil
	}
}

func buildConfig(options []Option) (config, error) {
	cfg := defaultConfig()

	for i, option := range options {
		if option == nil {
			return config{}, fmt.Errorf("agent/mcp: nil option at index %d", i)
		}

		if err := option(&cfg); err != nil {
			return config{}, err
		}
	}

	if cfg.clientOptions.CreateMessageHandler != nil && cfg.clientOptions.CreateMessageWithToolsHandler != nil {
		return config{}, errors.New("agent/mcp: client options set both sampling handlers")
	}

	return cfg, nil
}
