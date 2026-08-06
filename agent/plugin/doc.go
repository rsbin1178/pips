// Package plugin contains versioned semantic contracts for executable pips
// plugins.
//
// The package is a wire-contract boundary, not a Go implementation ABI. A
// released pips process must communicate with independently built plugin
// processes through a versioned package such as [v1]. Internal agent and
// internal/coding values do not cross this boundary.
package plugin
