// Package git attributes workspace changes relative to an in-memory-owned
// baseline using fixed read-only Git commands.
package git

import "errors"

var (
	// ErrNotRepository means the workspace is not inside a Git work tree.
	ErrNotRepository = errors.New("coding git changes: workspace is not a repository")
	// ErrSnapshotExpired means a snapshot is stale, consumed, foreign, or forged.
	ErrSnapshotExpired = errors.New("coding git changes: snapshot expired")
	// ErrLimit means a bounded scan, copy, command, or diff budget was exceeded.
	ErrLimit = errors.New("coding git changes: resource limit exceeded")
	// ErrGit means a fixed read-only Git command failed.
	ErrGit = errors.New("coding git changes: git command failed")
	// ErrClosed means an Inspector was used after Close.
	ErrClosed = errors.New("coding git changes: inspector closed")
)
