package coding

import (
	"fmt"

	"github.com/rsbin/pips/internal/coding/plandoc"
)

// PlanDocumentPath returns the application-derived private Plan path for
// frontend presentation. It does not create or read the document.
func (r *Runtime) PlanDocumentPath() (string, error) {
	if r == nil {
		return "", ErrRuntimeClosed
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return "", stateError("locate Plan document", phase, ErrRuntimeClosed)
	}

	repository := r.plans
	ref := r.planRef
	r.mu.Unlock()

	locator, ok := repository.(plandoc.Locator)
	if !ok {
		return "", fmt.Errorf("%w: Plan document location unavailable", ErrRuntimeInvalid)
	}

	return locator.Path(ref)
}
