//nolint:wsl_v5 // Page slicing and its bounds checks stay adjacent.
package coding

import (
	"context"
	"errors"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
)

// ErrHistoryUnavailable means the Runtime has no open session to read history from.
var ErrHistoryUnavailable = errors.New("coding: session history unavailable")

// History page bounds. A history read is a bounded backward walk over the durable
// session path, so a caller cannot ask for an unbounded payload.
const (
	HistoryPageDefaultLimit = 100
	HistoryPageMaxLimit     = 200
)

// TranscriptWindowMayBeTruncated reports whether the loaded conversation window is
// at the bootstrap cap, which means older durable messages may exist outside it.
//
// It is a hint rather than proof: a session with exactly the cap many messages
// reports true and has no older page, in which case the first history read simply
// returns nothing.
func TranscriptWindowMayBeTruncated(state State) bool {
	return len(state.Transcript) >= maxEventItems
}

// HistoryRequest asks for conversation messages older than the caller's window.
type HistoryRequest struct {
	// Before is the absolute index, counted from the oldest durable message, of the
	// first message the caller does not have. A negative value means "immediately
	// before the newest len(State.Transcript) messages", which is the window
	// BootstrapState delivered to a fresh frontend.
	Before int
	// Limit is the page size. Zero selects HistoryPageDefaultLimit; values above
	// HistoryPageMaxLimit are clamped.
	Limit int
}

// HistoryPage is one bounded, ordered slice of durable conversation history.
type HistoryPage struct {
	// Messages are ordered oldest first.
	Messages []ai.Message
	// Start is the absolute index of Messages[0] and Total is every durable
	// conversation message in the session.
	Start int
	Total int
	// More reports that a durable message exists older than Messages[0].
	More bool
}

// historyPage slices one page out of the durable path. It is pure so the paging
// arithmetic is testable without a session.
func historyPage(path []harness.Entry, loaded int, request HistoryRequest) HistoryPage {
	total := 0
	for _, entry := range path {
		if entry.Kind == harness.KindMessage && entry.Message != nil {
			total++
		}
	}

	end := request.Before
	if end < 0 {
		end = max(0, total-max(0, loaded))
	}
	end = min(max(0, end), total)

	limit := request.Limit
	if limit <= 0 {
		limit = HistoryPageDefaultLimit
	}
	limit = min(limit, HistoryPageMaxLimit)

	start := max(0, end-limit)
	page := HistoryPage{
		Messages: make([]ai.Message, 0, end-start),
		Start:    start,
		Total:    total,
		More:     start > 0,
	}

	seen := 0
	for _, entry := range path {
		if entry.Kind != harness.KindMessage || entry.Message == nil {
			continue
		}
		if seen >= start && seen < end {
			page.Messages = append(page.Messages, cloneMessage(entry.Message))
		}
		seen++
		if seen >= end {
			break
		}
	}

	return page
}

// History returns a bounded page of durable conversation history older than the
// caller's loaded window. It never mutates the session, the live State, or the
// model context: compaction may have removed messages from the context while they
// remain readable here.
func (r *Runtime) History(ctx context.Context, request HistoryRequest) (HistoryPage, error) {
	if err := ctx.Err(); err != nil {
		return HistoryPage{}, err
	}

	r.mu.Lock()
	session := r.session
	loaded := len(r.state.Transcript)
	r.mu.Unlock()

	if session == nil {
		return HistoryPage{}, ErrHistoryUnavailable
	}

	return historyPage(session.Path(), loaded, request), nil
}
