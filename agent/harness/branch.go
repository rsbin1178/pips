package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/rsbin/pips/ai"
)

const branchSummaryPrompt = `The messages above are a conversation branch that is being abandoned. Summarize it so the work is not lost when continuing from an earlier point.

Use this EXACT format:

## What was attempted
[The goal of this branch]

## Outcome
- [What was done, learned, or decided — including why the branch is being left]

## Reusable context
- [Anything worth carrying over, or "(none)"]

Keep it concise. Preserve exact names, paths, and error messages.`

// SummarizeBranch generates a summary of the branch being abandoned by a
// move from fromID back to toID (typically their common ancestor — see
// [Session.CommonAncestor]). Pass the result to [Session.MoveTo]. It fails
// with [ErrNothingToCompact] when the abandoned segment has no messages.
func SummarizeBranch(ctx context.Context, model ai.LanguageModel, sess *Session, fromID, toID string) (string, error) {
	path, err := sess.pathFrom(fromID)
	if err != nil {
		return "", err
	}

	// Keep only the segment past the destination.
	if toID != "" {
		for i, e := range path {
			if e.ID == toID {
				path = path[i+1:]
				break
			}
		}
	}

	var msgs ai.Messages
	for _, e := range path {
		msgs = append(msgs, entryContextMessages(e)...)
	}

	if len(msgs) == 0 {
		return "", ErrNothingToCompact
	}

	prompt := "<conversation>\n" + serializeConversation(msgs) + "\n</conversation>\n\n" + branchSummaryPrompt

	resp, err := model.Generate(ctx, ai.Request{
		Messages: ai.Messages{ai.SystemText(summarizationSystemPrompt), ai.UserText(prompt)},
	})
	if err != nil {
		return "", fmt.Errorf("harness: branch summarization: %w", err)
	}

	return strings.TrimSpace(resp.Text()), nil
}
