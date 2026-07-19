// Package agentmcp bridges tools from Model Context Protocol servers into the
// agent runtime using the official Go MCP SDK.
//
// Connect owns one initialized MCP client session. Tools returns an immutable
// snapshot suitable for agent.WithTools, while Session exposes the official
// SDK session for prompts, resources, completions, and other MCP features.
// Tool-list change signals tell applications when to obtain a new snapshot and
// construct the next Agent; an already-running Agent is never mutated.
//
// Remote tool metadata is untrusted. Use agent.WithBeforeTool to enforce
// application policy or human approval, and only wrap a returned tool with
// agent.Parallel after independently establishing that concurrent calls are
// safe.
package agentmcp
