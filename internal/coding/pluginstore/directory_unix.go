//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package pluginstore

import "fmt"

// syncCreatedDirectoryParent makes a newly created directory entry durable on
// Unix before the caller can publish metadata that depends on it.
func syncCreatedDirectoryParent(parent, created string) error {
	if err := syncDirectory(parent); err != nil {
		return &PublicationError{
			Operation: "directory-create",
			Path:      created,
			Committed: true,
			Err:       fmt.Errorf("sync directory parent: %w", err),
		}
	}

	return nil
}
