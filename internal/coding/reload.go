package coding

import (
	"errors"
	"fmt"
)

// ReloadPublicationError reports cleanup diagnostics after a new integration
// generation was already published. Its wrapped error is not a candidate
// construction failure: callers must keep using the published generation while
// deciding how to surface or retry retirement cleanup.
type ReloadPublicationError struct {
	GenerationID uint64
	Err          error
}

func (e *ReloadPublicationError) Error() string {
	if e == nil {
		return "coding reload: published"
	}

	if e.Err == nil {
		return fmt.Sprintf("coding reload: published generation %d", e.GenerationID)
	}

	return fmt.Sprintf(
		"coding reload: published generation %d; retired cleanup failed: %v",
		e.GenerationID, e.Err,
	)
}

func (e *ReloadPublicationError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// PublishedReloadGeneration reports whether err describes a reload whose
// candidate was already published. The returned generation ID is zero when
// err is nil or when the failure happened before publication.
func PublishedReloadGeneration(err error) (uint64, bool) {
	var published *ReloadPublicationError
	if !errors.As(err, &published) || published == nil {
		return 0, false
	}

	return published.GenerationID, true
}
