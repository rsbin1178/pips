// Package catalog provides the application composition boundary for agent
// tools. A Catalog holds explicitly registered local, MCP, and Team snapshots
// with provenance and risk. A Policy then creates a tenant-scoped, deny-by-
// default snapshot suitable for agent.WithTools.
//
// ToolSearch implements optional, source-aware deferred discovery. With
// ToolSearchOptions{Enabled: true}, local and Team tools stay direct while MCP
// and extension tools are deferred by default; DeferredSources customizes that
// policy. AgentOptions installs the direct snapshot and prepare hook. Raw
// files under a Skill's scripts/ folder are never discovered as tools.
package catalog
