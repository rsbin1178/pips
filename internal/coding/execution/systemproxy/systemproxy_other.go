//go:build !darwin

package systemproxy

import "context"

// Probe reports no operating-system HTTPS proxy. Only macOS stores one that
// Go's HTTP client ignores.
func Probe(context.Context) string { return "" }
