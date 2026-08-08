package acp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin/pips/internal/coding/execution"
	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
)

const sessionMCPConnectTimeout = 10 * time.Second

func convertMCPServers(servers []acpsdk.McpServer, maximum int) (codingmcp.Definitions, error) {
	if len(servers) > maximum {
		return codingmcp.Definitions{}, fmt.Errorf("%w: too many MCP servers", ErrInvalid)
	}

	values := make([]codingmcp.Definition, 0, len(servers))
	for index, server := range servers {
		if server.Stdio == nil || server.Http != nil || server.Sse != nil || server.Acp != nil {
			return codingmcp.Definitions{}, fmt.Errorf(
				"%w: MCP server %d must use stdio transport",
				ErrInvalid,
				index,
			)
		}

		stdio := server.Stdio
		if !validProtocolText(stdio.Name, 256, false) {
			return codingmcp.Definitions{}, fmt.Errorf("%w: MCP server %d has an invalid name", ErrInvalid, index)
		}

		environment := make([]execution.EnvVar, len(stdio.Env))
		for envIndex, variable := range stdio.Env {
			environment[envIndex] = execution.EnvVar{Name: variable.Name, Value: variable.Value}
		}

		values = append(values, codingmcp.Definition{
			ID: deterministicServerID(stdio.Name), Scope: codingmcp.ScopeSession,
			Transport: codingmcp.TransportStdio, Command: stdio.Command,
			Args: stdio.Args, Environment: environment, ConnectTimeout: sessionMCPConnectTimeout,
		})
	}

	definitions, err := codingmcp.NewDefinitions(values, maximum)
	if err != nil {
		return codingmcp.Definitions{}, fmt.Errorf("%w: MCP definitions", ErrInvalid)
	}

	return definitions, nil
}

func deterministicServerID(name string) string {
	var slug strings.Builder

	separator := false

	for _, character := range strings.ToLower(name) {
		if character <= unicode.MaxASCII && (unicode.IsLower(character) || unicode.IsDigit(character)) {
			if separator && slug.Len() > 0 {
				slug.WriteByte('-')
			}

			separator = false

			slug.WriteRune(character)

			if slug.Len() >= 31 {
				break
			}

			continue
		}

		separator = slug.Len() > 0
	}

	base := strings.Trim(slug.String(), "-")
	if base == "" {
		base = "server"
	}

	sum := sha256.Sum256([]byte(name))

	return base + "-" + hex.EncodeToString(sum[:6])
}

func validProtocolText(value string, maximum int, allowEmpty bool) bool {
	if (!allowEmpty && value == "") || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}

	for _, character := range value {
		if character == 0 {
			return false
		}
	}

	return true
}
