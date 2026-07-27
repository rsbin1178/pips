// Package gitcontrol runs the fixed Git plumbing operations required by the
// Coding Team Worktree control plane.
//
// It deliberately exposes typed operations instead of arbitrary process
// arguments. The package is trusted application infrastructure, not a
// model-callable shell or an Approval bypass.
package gitcontrol
