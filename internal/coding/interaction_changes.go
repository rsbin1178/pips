package coding

import (
	"context"
	"errors"
	"sync"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/changes/git"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// interactionChangeTracker delays an expensive Git baseline until the
// interaction first attempts a tool whose catalog contract can mutate state.
type interactionChangeTracker struct {
	mu        sync.Mutex
	inspector changes.Inspector
	risks     map[string]catalog.Risk

	hasAttemptedCapture bool
	hasBaseline         bool
	baseline            changes.Snapshot
	pendingDiagnostic   *IntegrationDiagnostic
}

func newInteractionChangeTracker(
	inspector changes.Inspector,
	descriptors []catalog.Descriptor,
) *interactionChangeTracker {
	risks := make(map[string]catalog.Risk, len(descriptors))
	for _, descriptor := range descriptors {
		risks[descriptor.Name] = descriptor.Risk
	}

	return &interactionChangeTracker{inspector: inspector, risks: risks}
}

func (t *interactionChangeTracker) beforeTool(
	ctx context.Context,
	info agent.ToolCallInfo,
) agent.ToolDecision {
	if t.canMutate(info.Name) {
		t.capture(ctx)
	}

	return agent.ToolDecision{}
}

func (t *interactionChangeTracker) preparePending(
	ctx context.Context,
	calls []ai.ToolCallPart,
) {
	for _, call := range calls {
		if t.canMutate(call.Name) {
			t.capture(ctx)

			return
		}
	}
}

func (t *interactionChangeTracker) canMutate(name string) bool {
	risk, ok := t.risks[name]

	return ok && risk >= catalog.RiskWrite
}

func (t *interactionChangeTracker) capture(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.hasAttemptedCapture {
		return
	}

	t.hasAttemptedCapture = true

	baseline, err := t.inspector.Capture(ctx)
	if err != nil {
		diagnostic := changeCaptureDiagnostic(err)
		t.pendingDiagnostic = &diagnostic

		return
	}

	t.baseline = baseline
	t.hasBaseline = true
}

func (t *interactionChangeTracker) finish(
	ctx context.Context,
) (changes.Report, bool) {
	t.mu.Lock()
	if !t.hasBaseline {
		t.mu.Unlock()

		return changes.Report{}, false
	}

	baseline := t.baseline
	t.hasBaseline = false
	t.mu.Unlock()

	report, err := t.inspector.Changes(ctx, baseline)
	if err != nil {
		diagnostic := changeReportDiagnostic(err)

		t.mu.Lock()
		t.pendingDiagnostic = &diagnostic
		t.mu.Unlock()

		return changes.Report{}, false
	}

	return report, true
}

func (t *interactionChangeTracker) drainDiagnostic() (IntegrationDiagnostic, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.pendingDiagnostic == nil {
		return IntegrationDiagnostic{}, false
	}

	diagnostic := *t.pendingDiagnostic
	t.pendingDiagnostic = nil

	return diagnostic, true
}

func changeCaptureDiagnostic(err error) IntegrationDiagnostic {
	code := "capture_failed"
	message := "workspace change attribution is unavailable for this interaction"

	switch {
	case errors.Is(err, git.ErrNotRepository):
		code = "not_repository"
	case errors.Is(err, workspace.ErrChanged):
		code = "capture_unstable"
		message = "workspace changed during baseline capture; change attribution is unavailable"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = "capture_timeout"
		message = "workspace baseline capture did not complete"
	case errors.Is(err, git.ErrLimit):
		code = "capture_limit"
		message = "workspace baseline capture exceeded its resource limit"
	case errors.Is(err, git.ErrGit):
		code = "capture_git_failed"
		message = "Git inspection failed during workspace baseline capture"
	}

	return IntegrationDiagnostic{
		Component: "changes", Code: code, Message: message, Disabled: true,
	}
}

func changeReportDiagnostic(err error) IntegrationDiagnostic {
	code := "report_failed"
	message := "workspace change attribution failed"

	switch {
	case errors.Is(err, workspace.ErrChanged):
		code = "report_unstable"
		message = "workspace changed during attribution; the change report was omitted"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = "report_timeout"
		message = "workspace change attribution did not complete"
	case errors.Is(err, git.ErrLimit):
		code = "report_limit"
		message = "workspace change attribution exceeded its resource limit"
	case errors.Is(err, git.ErrGit):
		code = "report_git_failed"
		message = "Git inspection failed during workspace change attribution"
	}

	return IntegrationDiagnostic{
		Component: "changes", Code: code, Message: message, Disabled: true,
	}
}
