// Package extension composes trusted, application-compiled Agent extensions
// into immutable runtime generations.
//
// An Extension prepares declarative contributions. A Runtime validates the
// complete set, starts its lifecycles, and only then atomically publishes a
// Snapshot. Acquired Activations pin their generation until Release, so a
// reload never mutates an in-flight Agent or Harness.
//
// This package is not a dynamic code loader or a process sandbox. Embedding
// applications decide which Go Extensions are compiled and registered.
package extension
