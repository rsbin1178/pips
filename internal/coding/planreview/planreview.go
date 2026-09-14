// Package planreview coordinates the explicit plan-mode decisions surfaced to
// the user: the enter approval and the exit review.
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

const (
	// maxNotesBytes bounds one revision-notes payload.
	maxNotesBytes = 16 << 10
	// maxComments bounds the number of review comments attached to an approval.
	maxComments = 128
	// maxCommentBytes bounds one review comment.
	maxCommentBytes = 4 << 10
	// maxCommentsBytes bounds all review comments together.
	maxCommentsBytes = 16 << 10
)

var (
	// ErrInvalid means a request or resolution violates the plan review protocol.
	ErrInvalid = errors.New("coding plan review: invalid value")
	// ErrNoPending means no plan decision is waiting for a resolution.
	ErrNoPending = errors.New("coding plan review: no pending request")
	// ErrMismatch means a resolution does not identify the pending request exactly.
	ErrMismatch = errors.New("coding plan review: request mismatch")
)

// Kind distinguishes the two plan-mode decisions.
type Kind string

// Plan-mode decision kinds.
const (
	// KindEnter asks the user to approve entering plan mode.
	KindEnter Kind = "enter"
	// KindExit presents the plan for approval, revision, or abandonment.
	KindExit Kind = "exit"
)

// Valid reports whether the kind is known.
func (k Kind) Valid() bool { return k == KindEnter || k == KindExit }

// Decision is the user's explicit disposition of a plan-mode request.
type Decision string

// Plan-mode decisions.
const (
	// DecisionApprove accepts the exit revision (or approves entering plan mode).
	DecisionApprove Decision = "approve"
	// DecisionDecline refuses to enter plan mode.
	DecisionDecline Decision = "decline"
	// DecisionRevise sends the plan back for another revision.
	DecisionRevise Decision = "revise"
	// DecisionQuit abandons the plan and turns plan mode off.
	DecisionQuit Decision = "quit"
)

// Request is one content-bound plan-mode decision identity. The plan content
// is read from disk when the request is created; it is never passed as a tool
// argument.
type Request struct {
	Kind       Kind   `json:"kind"`
	ID         string `json:"id"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content,omitempty"`
	HasContent bool   `json:"has_content"`
	Size       int64  `json:"size"`
}

// Resolution is one exact user decision for a pending request.
type Resolution struct {
	RequestID string   `json:"request_id"`
	Decision  Decision `json:"decision"`
	Comments  []string `json:"comments,omitempty"`
	Notes     string   `json:"notes,omitempty"`
}

// Outcome reports what the runtime must apply after a resolution.
type Outcome struct {
	Kind     Kind
	Decision Decision
}

// NewRequest binds one tool call to the plan content read from disk.
func NewRequest(kind Kind, toolCallID, content string) (Request, error) {
	if !kind.Valid() || !validIdentity(toolCallID) || !validContent(content) {
		return Request{}, fmt.Errorf("%w: invalid request identity", ErrInvalid)
	}

	sum := sha256.Sum256([]byte(string(kind) + "\x00" + toolCallID))

	return Request{
		Kind:       kind,
		ID:         "plan-" + hex.EncodeToString(sum[:]),
		ToolCallID: toolCallID,
		Content:    content,
		HasContent: strings.TrimSpace(content) != "",
		Size:       int64(len(content)),
	}, nil
}

// CloneRequest returns an independent request value.
func CloneRequest(request Request) Request { return request }

// CloneResolution returns an independent resolution value.
func CloneResolution(resolution Resolution) Resolution {
	resolution.Comments = append([]string(nil), resolution.Comments...)

	return resolution
}

// ValidateRequest verifies a request's deterministic identity and bounds.
func ValidateRequest(request Request) error {
	if !request.Kind.Valid() || !validIdentity(request.ID) || !validIdentity(request.ToolCallID) {
		return fmt.Errorf("%w: invalid request identity", ErrInvalid)
	}

	want, err := NewRequest(request.Kind, request.ToolCallID, request.Content)
	if err != nil {
		return err
	}

	if request.ID != want.ID || request.HasContent != want.HasContent || request.Size != want.Size {
		return fmt.Errorf("%w: request digest mismatch", ErrInvalid)
	}

	return nil
}

// ValidateResolution verifies one decision against the exact pending request.
func ValidateResolution(request Request, resolution Resolution) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}

	return ValidateResolutionShape(request.Kind, resolution, request.ID)
}

// ValidDecision reports whether one decision is well-formed for a request kind.
// It validates the shape only; comments and notes are bounded by
// [ValidateResolutionShape].
func ValidDecision(kind Kind, decision Decision) bool {
	switch kind {
	case KindEnter:
		return decision == DecisionApprove || decision == DecisionDecline
	case KindExit:
		return decision == DecisionApprove || decision == DecisionRevise || decision == DecisionQuit
	default:
		return false
	}
}

// ValidateResolutionShape verifies bounded fields for one request kind.
func ValidateResolutionShape(kind Kind, resolution Resolution, requestID string) error {
	if !kind.Valid() || requestID == "" || resolution.RequestID != requestID {
		return fmt.Errorf("%w: resolution does not match request", ErrInvalid)
	}

	switch kind {
	case KindEnter:
		return validateEnterResolution(resolution)
	case KindExit:
		return validateExitResolution(resolution)
	default:
		return fmt.Errorf("%w: unsupported request kind", ErrInvalid)
	}
}

func validateEnterResolution(resolution Resolution) error {
	switch resolution.Decision {
	case DecisionApprove, DecisionDecline:
		if len(resolution.Comments) > 0 || resolution.Notes != "" {
			return fmt.Errorf("%w: entry decisions carry no comments", ErrInvalid)
		}

		return nil
	default:
		return fmt.Errorf("%w: unsupported entry decision", ErrInvalid)
	}
}

func validateExitResolution(resolution Resolution) error {
	switch resolution.Decision {
	case DecisionApprove:
		if strings.TrimSpace(resolution.Notes) != "" {
			return fmt.Errorf("%w: approval cannot include revision notes", ErrInvalid)
		}

		return validateComments(resolution.Comments)
	case DecisionRevise:
		if len(resolution.Comments) > 0 {
			return fmt.Errorf("%w: revision requests carry notes only", ErrInvalid)
		}
		if !validOptionalText(resolution.Notes, maxNotesBytes) {
			return fmt.Errorf("%w: revision notes are too large or invalid", ErrInvalid)
		}

		return nil
	case DecisionQuit:
		if len(resolution.Comments) > 0 || resolution.Notes != "" {
			return fmt.Errorf("%w: abandoning carries no feedback", ErrInvalid)
		}

		return nil
	default:
		return fmt.Errorf("%w: unsupported exit decision", ErrInvalid)
	}
}

func validateComments(comments []string) error {
	if len(comments) > maxComments {
		return fmt.Errorf("%w: too many review comments", ErrInvalid)
	}

	total := 0
	for _, comment := range comments {
		if !validOptionalText(comment, maxCommentBytes) {
			return fmt.Errorf("%w: review comment is too large or invalid", ErrInvalid)
		}

		total += len(comment)
	}

	if total > maxCommentsBytes {
		return fmt.Errorf("%w: review comments are too large", ErrInvalid)
	}

	return nil
}

func validContent(value string) bool {
	return utf8.ValidString(value) && len(value) <= 1<<20 && !strings.ContainsRune(value, '\x00')
}

func validIdentity(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value &&
		!strings.ContainsRune(value, '\x00') && utf8.ValidString(value)
}

func validOptionalText(value string, maximum int) bool {
	return len(value) <= maximum && !strings.ContainsRune(value, '\x00') && utf8.ValidString(value)
}
