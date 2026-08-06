//nolint:wsl_v5 // PublicationError keeps outcome fields adjacent to its formatting.
package pluginstore

import (
	"errors"
	"fmt"
)

// PublicationError reports an operation whose filesystem publication may have
// completed even though a later durability or deferred-cleanup step failed.
// Callers must inspect Committed/Published and retry by reading the current
// store state rather than assuming that no candidate was installed or that a
// lease/record is still held. A committed lease-marker removal is logically
// released even when Release returns this error.
type PublicationError struct {
	Operation string
	Path      string
	PluginID  string
	Record    *InstallRecord
	Committed bool
	Published bool
	Err       error
}

func (e *PublicationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	status := "not committed"
	if e.Committed {
		status = "committed"
	}
	if e.Published {
		status += ", published"
	}
	return fmt.Sprintf("coding plugin store: %s at %s (%s): %v", e.Operation, e.Path, status, e.Err)
}

func (e *PublicationError) Unwrap() error { return e.Err }

var (
	// ErrInvalid identifies malformed manifest, install record, or request data.
	ErrInvalid = errors.New("coding plugin store: invalid value")
	// ErrUnsupportedSchema identifies a manifest or record schema this binary does not understand.
	ErrUnsupportedSchema = errors.New("coding plugin store: unsupported schema")
	// ErrUnsupportedTarget identifies a target outside the supported platform set or not present in a manifest.
	ErrUnsupportedTarget = errors.New("coding plugin store: unsupported target")
	// ErrDuplicate identifies a duplicate manifest or install-record identity.
	ErrDuplicate = errors.New("coding plugin store: duplicate value")
	// ErrLimitExceeded identifies a bounded input or artifact that is too large.
	ErrLimitExceeded = errors.New("coding plugin store: limit exceeded")
	// ErrUnsafeArtifact identifies a symlink, special file, or replaced artifact.
	ErrUnsafeArtifact = errors.New("coding plugin store: unsafe artifact")
	// ErrDigestMismatch identifies bytes that do not match the manifest digest.
	ErrDigestMismatch = errors.New("coding plugin store: digest mismatch")
	// ErrNotFound identifies a missing installed record or enablement.
	ErrNotFound = errors.New("coding plugin store: not found")
	// ErrNotEnabled identifies a plugin with no current enablement reference.
	ErrNotEnabled = errors.New("coding plugin store: plugin is not enabled")
	// ErrConflict identifies a concurrent or conflicting filesystem update.
	ErrConflict = errors.New("coding plugin store: conflict")
	// ErrEnabled identifies an artifact that is still the current enablement.
	ErrEnabled = errors.New("coding plugin store: artifact is enabled")
	// ErrDurability identifies a committed replacement whose directory sync failed.
	ErrDurability = errors.New("coding plugin store: durability uncertain")
	// ErrArtifactLeased identifies an artifact still pinned by a generation/process owner.
	ErrArtifactLeased = errors.New("coding plugin store: artifact is leased")
)
