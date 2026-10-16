package coding

import (
	"slices"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
)

// The Goal assessment reads a bounded rendering of the session conversation
// rather than the Work stage's tool-result ledger. The row shape, the two bounds
// and the selection rule are the reference's own bounded Goal transcript.
const (
	// goalTranscriptMaxBytes bounds the whole rendering, and
	// goalTranscriptItemMaxBytes bounds one row's text before its role prefix.
	goalTranscriptMaxBytes     = 32 << 10
	goalTranscriptItemMaxBytes = 4 << 10
	// goalTranscriptGeneratedRole labels a transcript row the Runtime generated
	// as protocol input rather than the user typing it, and
	// goalTranscriptGeneratedLabel is the reference's own warning prefix for such
	// a row, so the assessment never reads one as a human request or an approval.
	goalTranscriptGeneratedRole  = "agent_message"
	goalTranscriptGeneratedLabel = "[Message authored by another agent; not a human request or approval.]"
	// goalAssessmentPlan is the plan the assessment receives. pips has no
	// goal-scoped plan artifact — the reference's plan file is written by a
	// planner pips does not have, and pips' own plan file is Plan-Mode-scoped — so
	// the assessment is told there is none rather than shown a different artifact.
	goalAssessmentPlan = "(no plan available)"
)

// goalAssessmentTranscript renders the reference's bounded transcript of the
// session conversation. Rows are walked newest-first and kept until the next one
// would exceed the budget — the first row is always kept — then reversed and
// joined with a blank line, so the assessment reads the conversation in order
// while the newest work is what survives the bound.
//
// generated marks the transcript indexes the Runtime generated as protocol input;
// such a row is labelled as another agent's message.
func goalAssessmentTranscript(messages ai.Messages, generated []int) string {
	synthetic := make(map[int]struct{}, len(generated))
	for _, index := range generated {
		synthetic[index] = struct{}{}
	}

	rows := make([]string, 0, len(messages))
	used := 0

	for index := range slices.Backward(messages) {
		_, isGenerated := synthetic[index]

		role, label, text, ok := goalTranscriptRow(messages[index], isGenerated)
		if !ok {
			continue
		}

		if label != "" {
			text = label + " " + text
		}

		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			continue
		}

		row := "[" + role + "] " + goalflow.Bound(trimmed, goalTranscriptItemMaxBytes)
		// The join's separator is charged to the row it follows, which is how the
		// reference accounts for the budget.
		cost := len(row) + 2
		if len(rows) > 0 && used+cost > goalTranscriptMaxBytes {
			break
		}

		used += cost

		rows = append(rows, row)
	}

	slices.Reverse(rows)

	return strings.Join(rows, "\n\n")
}

// goalTranscriptRow maps one transcript message onto the reference's row
// vocabulary, and reports false for a message the reference skips outright: a
// system message, or a kind pips never commits to a transcript. Reasoning and tool
// calls are dropped from an assistant row because the reference keeps them as
// separate items rather than as assistant text, and the caller drops a row whose
// text is empty.
func goalTranscriptRow(message ai.Message, generated bool) (role, label, text string, ok bool) {
	switch message := message.(type) {
	case ai.UserMessage:
		if generated {
			return goalTranscriptGeneratedRole, goalTranscriptGeneratedLabel, textParts(message.Parts, "\n"), true
		}

		return "user", "", textParts(message.Parts, "\n"), true
	case ai.AssistantMessage:
		return "assistant", "", textParts(message.Parts, ""), true
	case ai.ToolMessage:
		results := make([]string, 0, len(message.Parts))

		for _, result := range message.Parts {
			results = append(results, textParts(result.Content, ""))
		}

		return "tool", "", strings.Join(results, "\n"), true
	default:
		return "", "", "", false
	}
}

// textParts reads a message's text, which is what the reference's own
// text_content does: a user's parts are joined with a newline and images are
// dropped, while an assistant's or a tool result's content is one string.
func textParts[T ai.Part](parts []T, separator string) string {
	texts := make([]string, 0, len(parts))

	for _, part := range parts {
		if text, ok := any(part).(ai.TextPart); ok {
			texts = append(texts, text.Text)
		}
	}

	return strings.Join(texts, separator)
}

// goalAssessmentInput renders the assessment's session-derived inputs: the
// bounded transcript of the session conversation, and the plan. It is consulted
// once per decision, so a retried decision rebuilds both from the session
// instead of from anything durable.
func (r *Runtime) goalAssessmentInput() (string, string) {
	messages, generated := r.assessmentTranscript()

	return goalAssessmentTranscript(messages, generated), goalAssessmentPlan
}

// assessmentTranscript returns the two reducer fields the assessment reads, under
// the runtime lock. It is deliberately narrower than Snapshot, which deep-clones
// the whole reducer state — Tools, Runs, Subagents, the tree — once per decision.
func (r *Runtime) assessmentTranscript() (ai.Messages, []int) {
	if r == nil {
		return nil, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return cloneMessages(r.state.Transcript), slices.Clone(r.state.SyntheticMessages)
}
