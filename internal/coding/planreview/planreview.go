// Package planreview coordinates explicit review of one session-bound Plan.
//
//nolint:wsl_v5 // Closed protocol validation keeps identity checks adjacent.
package planreview

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxFeedbackBytes = 16 << 10

var (
	// ErrInvalid means a request or resolution violates the Plan review protocol.
	ErrInvalid = errors.New("coding plan review: invalid value")
	// ErrNoPending means no submit_plan call is waiting for a decision.
	ErrNoPending = errors.New("coding plan review: no pending request")
	// ErrMismatch means a decision does not identify the pending request exactly.
	ErrMismatch = errors.New("coding plan review: request mismatch")
)

// Decision is the user's explicit disposition of a submitted Plan.
type Decision string

const (
	// DecisionContinue resumes the same Plan interaction for revisions.
	DecisionContinue Decision = "continue_planning"
	// DecisionApprove accepts the exact revision and requests Agent Mode at idle.
	DecisionApprove Decision = "approve_agent_mode"
)

// Arguments are the complete provider-visible submit_plan input.
type Arguments struct {
	ExpectedRevision string `json:"expected_revision"`
}

// Request is the content-free identity of one submitted Plan revision.
type Request struct {
	ID         string `json:"id"`
	ToolCallID string `json:"tool_call_id"`
	Revision   string `json:"revision"`
	Size       int64  `json:"size"`
}

// Resolution is one exact user decision for a submitted Plan.
type Resolution struct {
	RequestID string   `json:"request_id"`
	Revision  string   `json:"revision"`
	Decision  Decision `json:"decision"`
	Feedback  string   `json:"feedback,omitempty"`
}

// NewRequest deterministically binds a Tool call to the submitted revision.
func NewRequest(toolCallID, revision string, size int64) (Request, error) {
	if !validIdentity(toolCallID) || !validRevision(revision) || size < 0 {
		return Request{}, fmt.Errorf("%w: invalid request identity", ErrInvalid)
	}

	sum := sha256.Sum256([]byte(toolCallID + "\x00" + revision))

	return Request{
		ID:         "plan-" + hex.EncodeToString(sum[:]),
		ToolCallID: toolCallID,
		Revision:   revision,
		Size:       size,
	}, nil
}

// ValidateRequest verifies a request's deterministic identity and bounds.
func ValidateRequest(request Request) error {
	want, err := NewRequest(request.ToolCallID, request.Revision, request.Size)
	if err != nil {
		return err
	}
	if request.ID != want.ID {
		return fmt.Errorf("%w: request digest mismatch", ErrInvalid)
	}

	return nil
}

// ValidateResolution verifies one decision against the exact pending request.
func ValidateResolution(request Request, resolution Resolution) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}
	if resolution.RequestID != request.ID || resolution.Revision != request.Revision {
		return fmt.Errorf("%w: resolution does not match request", ErrInvalid)
	}

	return ValidateResolutionShape(resolution)
}

// ValidateResolutionShape verifies bounded fields without a pending request.
func ValidateResolutionShape(resolution Resolution) error {
	if !validIdentity(resolution.RequestID) || !validRevision(resolution.Revision) {
		return fmt.Errorf("%w: invalid resolution identity", ErrInvalid)
	}

	switch resolution.Decision {
	case DecisionContinue:
		if !validOptionalText(resolution.Feedback, maxFeedbackBytes) {
			return fmt.Errorf("%w: feedback is too large or invalid", ErrInvalid)
		}
	case DecisionApprove:
		if resolution.Feedback != "" {
			return fmt.Errorf("%w: approval cannot include feedback", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported decision", ErrInvalid)
	}

	return nil
}

// CloneRequest returns an independent request value.
func CloneRequest(request Request) Request { return request }

// CloneResolution returns an independent resolution value.
func CloneResolution(resolution Resolution) Resolution { return resolution }

func validRevision(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)

	return err == nil
}

func validIdentity(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value &&
		!strings.ContainsRune(value, '\x00') && utf8.ValidString(value)
}

func validOptionalText(value string, maximum int) bool {
	return len(value) <= maximum && !strings.ContainsRune(value, '\x00') && utf8.ValidString(value)
}
