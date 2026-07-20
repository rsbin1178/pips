package approval

import "errors"

var (
	// ErrApprovalRequired means a controlled operation needs an interactive decision.
	ErrApprovalRequired = &Error{message: "coding approval: user approval required", code: "approval_required"}
	// ErrOutcomeUnknown means an earlier operation may have run and cannot be replayed automatically.
	ErrOutcomeUnknown = &Error{message: "coding approval: command outcome is unknown", code: "outcome_unknown"}
	// ErrInvalidResolution means the decision is stale or invalid for the current state.
	ErrInvalidResolution = &Error{message: "coding approval: invalid resolution", code: "invalid_resolution"}
	// ErrJournalCorrupt means approval lifecycle data failed strict replay.
	ErrJournalCorrupt = &Error{message: "coding approval: journal lifecycle is malformed", code: "journal_corrupt"}
	// ErrDenied means policy or the user rejected the operation.
	ErrDenied = &Error{message: "coding approval: command denied", code: "approval_denied"}
)

// Error is a stable approval error with a coding-tool result code.
type Error struct {
	message string
	code    string
}

func (e *Error) Error() string { return e.message }

// ToolErrorCode lets coding tools preserve the stable error class without a
// package dependency on approval.
func (e *Error) ToolErrorCode() string { return e.code }

// Is supports errors.Is against the package sentinels.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)

	return ok && e.code == other.code
}

func wrapError(target *Error, detail string) error {
	if detail == "" {
		return target
	}

	return errors.Join(target, errors.New(detail))
}
