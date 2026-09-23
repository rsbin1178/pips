// Package catalog provides the application composition boundary for agent
// tools. A Catalog holds explicitly registered local, MCP, and Team snapshots
// with provenance and risk. A Policy then creates a tenant-scoped, deny-by-
// default snapshot suitable for agent.WithTools.
//
// [Merge] combines several immutable Catalogs while preserving their entry
// order and policy metadata. Duplicate tool names remain errors rather than
// receiving an implicit precedence.
//
// ToolSearch implements optional, source-aware deferred discovery. With
// ToolSearchOptions{Enabled: true}, local and Team tools stay direct while MCP
// and extension tools are deferred by default; DeferredSources customizes that
// policy. The discovery tool is named DefaultToolSearchName unless Name
// overrides it, is only advertised while at least one authorized deferred tool
// exists, and ranks Search results by name, parameter, and description hits.
// AgentOptions installs the executable snapshot and prepare hook. Raw files
// under a Skill's scripts/ folder are never discovered as tools.
package catalog
