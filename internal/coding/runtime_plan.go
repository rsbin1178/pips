//nolint:wsl_v5 // Runtime lease checks and exact-revision reads stay adjacent.
package coding

import (
	"context"
	"errors"
	"fmt"

	"github.com/rsbin/pips/internal/coding/plandoc"
)

// PlanDocument is the bounded path-free Plan view exposed to frontends.
type PlanDocument struct {
	Revision string `json:"revision"`
	Content  string `json:"content"`
	Size     int64  `json:"size"`
}

// ReadPlanDocument returns the exact requested current Plan revision without
// disclosing its private storage path.
func (r *Runtime) ReadPlanDocument(
	ctx context.Context,
	expectedRevision string,
) (PlanDocument, error) {
	if r == nil {
		return PlanDocument{}, ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return PlanDocument{}, fmt.Errorf("%w: Team Worker has no Plan document", ErrRuntimeInvalid)
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return PlanDocument{}, stateError("read Plan document", phase, ErrRuntimeClosed)
	}
	repository := r.plans
	ref := r.planRef
	r.mu.Unlock()

	document, err := repository.Read(ctx, ref)
	if err != nil {
		return PlanDocument{}, fmt.Errorf("%w: Plan document unavailable", ErrRuntimeInvalid)
	}
	if expectedRevision == "" || document.Revision != expectedRevision {
		return PlanDocument{}, errors.Join(ErrRuntimeInvalid, plandoc.ErrConflict)
	}

	return PlanDocument{
		Revision: document.Revision,
		Content:  document.Content,
		Size:     document.Size,
	}, nil
}

// PlanDocumentPath returns the application-derived private Plan path for
// frontend presentation. It does not create or read the document.
func (r *Runtime) PlanDocumentPath() (string, error) {
	if r == nil {
		return "", ErrRuntimeClosed
	}
	if r.isTeamWorker() {
		return "", fmt.Errorf("%w: Team Worker has no Plan document", ErrRuntimeInvalid)
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
