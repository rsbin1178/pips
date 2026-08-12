//nolint:wsl_v5 // Token accounting stages intentionally stay grouped.
package harness

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/ai"
)

// Default compaction settings (mirroring pi's harness defaults).
const (
	DefaultCompactionReserveTokens    = 16384
	DefaultCompactionKeepRecentTokens = 20000
	// DefaultCompactionSummaryTokens bounds summary generation independently
	// from the output space reserved for the next conversation turn.
	DefaultCompactionSummaryTokens = 4096
)

// CompactionSettings tunes automatic compaction. ContextTokens must be set to the
// model's context-window budget — the ai package deliberately has no
// per-model window table.
type CompactionSettings struct {
	// ContextTokens is the total context budget compaction defends.
	ContextTokens int
	// ReserveTokens is kept free for the summarization prompt and the next
	// turn's output (default [DefaultCompactionReserveTokens]).
	ReserveTokens int
	// KeepRecentTokens is approximately how much recent history survives a
	// compaction (default [DefaultCompactionKeepRecentTokens]).
	KeepRecentTokens int
	// SummaryTokens is the maximum output budget for the generated summary
	// (default [DefaultCompactionSummaryTokens]). It is deliberately distinct
	// from ReserveTokens, which protects the following normal model request.
	SummaryTokens int
}

func (s CompactionSettings) withDefaults() CompactionSettings {
	if s.ReserveTokens <= 0 {
		s.ReserveTokens = DefaultCompactionReserveTokens
	}

	if s.KeepRecentTokens <= 0 {
		s.KeepRecentTokens = DefaultCompactionKeepRecentTokens
	}
	if s.SummaryTokens <= 0 {
		s.SummaryTokens = DefaultCompactionSummaryTokens
	}

	return s
}

// ShouldCompact reports whether an estimated context size crosses the
// compaction threshold.
func ShouldCompact(tokens int, s CompactionSettings) bool {
	s = s.withDefaults()

	return s.ContextTokens > s.ReserveTokens && tokens > s.ContextTokens-s.ReserveTokens
}

// estimatedImageTokens approximates one image as 4800 characters, matching
// pi's heuristic.
const estimatedImageChars = 4800

// EstimateTokens estimates one message's token count with a conservative
// four-characters-per-token heuristic.
func EstimateTokens(msg ai.Message) int {
	chars := 0

	parts, err := ai.MessageParts(msg)
	if err != nil {
		return 0
	}

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			chars += len(p.Text)
		case ai.ReasoningPart:
			chars += len(p.Text)
		case ai.ImagePart:
			chars += estimatedImageChars
		case ai.ToolCallPart:
			chars += len(p.Name) + len(p.Args)
		case ai.ToolResultPart:
			chars += partsChars(p.Content)
		}
	}

	return (chars + 3) / 4
}

func partsChars(parts []ai.Part) int {
	chars := 0

	for _, part := range parts {
		switch p := part.(type) {
		case ai.TextPart:
			chars += len(p.Text)
		case ai.ImagePart:
			chars += estimatedImageChars
		default:
		}
	}

	return chars
}

// EstimateContext estimates the context size of a branch: the last recorded
// assistant usage (provider-reported input plus output) plus the
// heuristically estimated messages after it.
func EstimateContext(path []Entry) int {
	contextPath := contextEntries(path)
	contextIDs := make(map[string]struct{}, len(contextPath))
	for _, entry := range contextPath {
		contextIDs[entry.ID] = struct{}{}
	}
	lastUsage := -1

	for i, e := range path {
		if e.Kind == KindMessage && e.Usage != nil {
			if _, visible := contextIDs[e.ID]; !visible {
				continue
			}
			lastUsage = i
		}
	}

	tokens := 0
	if lastUsage >= 0 {
		tokens = path[lastUsage].Usage.InputTokens + path[lastUsage].Usage.OutputTokens
	}

	for _, e := range contextPath {
		if lastUsage >= 0 && !entryAfter(path, e.ID, lastUsage) {
			continue
		}

		for _, msg := range entryContextMessages(e) {
			tokens += EstimateTokens(msg)
		}
	}

	return tokens
}

// entryAfter reports whether the entry with the given ID sits after index
// in the path.
func entryAfter(path []Entry, id string, index int) bool {
	for _, e := range path[index+1:] {
		if e.ID == id {
			return true
		}
	}

	return false
}

// entryContextMessages renders an entry's model-visible messages.
func entryContextMessages(e Entry) ai.Messages {
	switch e.Kind {
	case KindMessage:
		return ai.Messages{e.Message}
	case KindCompaction:
		return ai.Messages{ai.UserText(CompactionPrefix + e.Summary)}
	case KindBranchSummary:
		return ai.Messages{ai.UserText(BranchSummaryPrefix + e.Summary)}
	default:
		return nil
	}
}

// CompactionPlan is a planned compaction, produced by [PlanCompaction] and consumed by
// [SummarizeCompaction].
type CompactionPlan struct {
	// FirstKeptID is the entry where retained history starts.
	FirstKeptID string
	// ToSummarize is the history being folded into the summary.
	ToSummarize ai.Messages
	// TurnPrefix holds the leading messages of a split turn, summarized
	// separately (see SplitTurn).
	TurnPrefix ai.Messages
	// SplitTurn reports that the cut lands inside a turn: its prefix is
	// summarized while its tail is retained.
	SplitTurn bool
	// TokensBefore is the estimated context size before compaction.
	TokensBefore int
	// Previous is the prior compaction's summary, updated iteratively.
	Previous string
}

// PlanCompaction plans a compaction of the branch: it finds the cut point that
// keeps roughly KeepRecentTokens of recent history (never separating a tool
// result from its call) and collects the messages to summarize. It returns
// nil when there is nothing to compact.
func PlanCompaction(path []Entry, settings CompactionSettings) *CompactionPlan {
	settings = settings.withDefaults()

	if len(path) == 0 || path[len(path)-1].Kind == KindCompaction {
		return nil
	}

	start, previous := compactionBoundary(path)
	cut := findCutPoint(path, start, settings.KeepRecentTokens)

	if cut.firstKept <= start {
		return nil // nothing would be summarized
	}

	prep := &CompactionPlan{
		FirstKeptID:  path[cut.firstKept].ID,
		SplitTurn:    cut.splitTurn,
		TokensBefore: EstimateContext(path),
		Previous:     previous,
	}

	historyEnd := cut.firstKept
	if cut.splitTurn {
		historyEnd = cut.turnStart
	}

	for _, e := range path[start:historyEnd] {
		if e.Kind == KindCompaction {
			continue
		}

		prep.ToSummarize = append(prep.ToSummarize, entryContextMessages(e)...)
	}

	if cut.splitTurn {
		for _, e := range path[cut.turnStart:cut.firstKept] {
			prep.TurnPrefix = append(prep.TurnPrefix, entryContextMessages(e)...)
		}
	}

	return prep
}

// compactionBoundary locates where compactable history starts: after the
// previous compaction's retained boundary, carrying its summary forward.
func compactionBoundary(path []Entry) (start int, previous string) {
	for i, entry := range slices.Backward(path) {
		if entry.Kind != KindCompaction {
			continue
		}

		previous = entry.Summary

		for j, e := range path {
			if e.ID == entry.FirstKeptID {
				return j, previous
			}
		}

		return i + 1, previous
	}

	return 0, ""
}

type cutPoint struct {
	firstKept int
	turnStart int
	splitTurn bool
}

// isCutCandidate reports whether an entry may start the retained history.
// Tool-result messages never qualify: cutting there would separate them
// from their calls.
func isCutCandidate(e Entry) bool {
	switch e.Kind {
	case KindBranchSummary:
		return true
	case KindMessage:
		switch e.Message.(type) {
		case ai.UserMessage, ai.AssistantMessage:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

// findCutPoint walks back accumulating estimated tokens until roughly
// keepRecent is retained, then picks the nearest valid cut at or after that
// position (mirroring pi's algorithm, including split-turn detection).
func findCutPoint(path []Entry, start, keepRecent int) cutPoint {
	var candidates []int

	for i := start; i < len(path); i++ {
		if isCutCandidate(path[i]) {
			candidates = append(candidates, i)
		}
	}

	if len(candidates) == 0 {
		return cutPoint{firstKept: start, turnStart: -1}
	}

	cut := candidates[0]
	accumulated := 0

	for i := len(path) - 1; i >= start; i-- {
		if path[i].Kind != KindMessage {
			continue
		}

		accumulated += EstimateTokens(path[i].Message)
		if accumulated < keepRecent {
			continue
		}

		for _, c := range candidates {
			if c >= i {
				cut = c
				break
			}
		}

		break
	}

	entry := path[cut]
	_, isUser := entry.Message.(ai.UserMessage)
	isUser = entry.Kind == KindMessage && isUser

	if isUser {
		return cutPoint{firstKept: cut, turnStart: -1}
	}

	turnStart := findTurnStart(path, cut, start)

	return cutPoint{firstKept: cut, turnStart: turnStart, splitTurn: turnStart >= 0}
}

// findTurnStart walks back to the user message (or branch summary) that
// opened the turn containing the given index; -1 when none exists.
func findTurnStart(path []Entry, index, start int) int {
	for i := index; i >= start; i-- {
		e := path[i]
		if e.Kind == KindBranchSummary {
			return i
		}

		if _, isUser := e.Message.(ai.UserMessage); e.Kind == KindMessage && isUser {
			return i
		}
	}

	return -1
}

// Summarization prompts, ported from pi's compaction verbatim (structure
// preserved so summaries stay interchangeable in spirit).
const (
	summarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

	summarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

	updateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Keep each section concise. Preserve exact file paths, function names, and error messages.`

	turnPrefixPrompt = `This is the PREFIX of a turn that was too large to keep. The SUFFIX (recent work) is retained.

Summarize the prefix to provide context for the retained suffix:

## Original Request
[What did the user ask for in this turn?]

## Early Progress
- [Key decisions and work done in the prefix]

## Context for Suffix
- [Information needed to understand the retained recent work]

Be concise. Focus on what's needed to understand the kept suffix.`
)

// SummarizeCompaction generates the compaction summary for a prepared plan using the
// given model: the history summary (updating a previous one when present),
// plus a separately summarized turn prefix for split turns. instructions
// optionally focus the summary. Commit the result with
// [Session.AppendCompaction].
func SummarizeCompaction(ctx context.Context, model ai.LanguageModel, prep *CompactionPlan, settings CompactionSettings, instructions string) (string, error) {
	settings = settings.withDefaults()

	summary := "No prior history."

	if len(prep.ToSummarize) > 0 {
		var err error

		summary, err = summarizeMessages(ctx, model, prep.ToSummarize, summaryRequest{
			maxTokens:    settings.SummaryTokens,
			previous:     prep.Previous,
			instructions: instructions,
		})
		if err != nil {
			return "", err
		}
	}

	if !prep.SplitTurn || len(prep.TurnPrefix) == 0 {
		return summary, nil
	}

	prefix, err := summarizeMessages(ctx, model, prep.TurnPrefix, summaryRequest{
		maxTokens:  max(settings.SummaryTokens/2, 1),
		turnPrefix: true,
	})
	if err != nil {
		return "", err
	}

	return summary + "\n\n---\n\n**Turn Context (split turn):**\n\n" + prefix, nil
}

type summaryRequest struct {
	maxTokens    int
	previous     string
	instructions string
	turnPrefix   bool
}

func summarizeMessages(ctx context.Context, model ai.LanguageModel, msgs ai.Messages, req summaryRequest) (string, error) {
	var b strings.Builder

	b.WriteString("<conversation>\n")
	b.WriteString(serializeConversation(msgs))
	b.WriteString("\n</conversation>\n\n")

	if req.previous != "" {
		b.WriteString("<previous-summary>\n")
		b.WriteString(req.previous)
		b.WriteString("\n</previous-summary>\n\n")
	}

	switch {
	case req.turnPrefix:
		b.WriteString(turnPrefixPrompt)
	case req.previous != "":
		b.WriteString(updateSummarizationPrompt)
	default:
		b.WriteString(summarizationPrompt)
	}

	if req.instructions != "" {
		b.WriteString("\n\nAdditional focus: ")
		b.WriteString(req.instructions)
	}

	resp, err := model.Generate(ctx, ai.Request{
		Messages:  ai.Messages{ai.SystemText(summarizationSystemPrompt), ai.UserText(b.String())},
		MaxTokens: ai.Ptr(req.maxTokens),
	})
	if err != nil {
		return "", fmt.Errorf("harness: summarization: %w", err)
	}

	return resp.Text(), nil
}

// serializeConversation renders messages as plain text for summarization
// prompts.
func serializeConversation(msgs ai.Messages) string {
	var b strings.Builder

	for _, msg := range msgs {
		b.WriteByte('[')
		b.WriteString(messageLabel(msg))
		b.WriteString("]\n")

		parts, err := ai.MessageParts(msg)
		if err != nil {
			continue
		}

		for _, part := range parts {
			switch p := part.(type) {
			case ai.TextPart:
				b.WriteString(p.Text)
				b.WriteByte('\n')
			case ai.ToolCallPart:
				b.WriteString("tool call ")
				b.WriteString(p.Name)
				b.WriteByte('(')
				b.WriteString(string(p.Args))
				b.WriteString(")\n")
			case ai.ToolResultPart:
				b.WriteString("tool result ")
				b.WriteString(p.Name)
				b.WriteString(": ")

				for _, c := range p.Content {
					if t, ok := c.(ai.TextPart); ok {
						b.WriteString(t.Text)
					}
				}

				b.WriteByte('\n')
			case ai.ImagePart:
				b.WriteString("[image]\n")
			default:
			}
		}

		b.WriteByte('\n')
	}

	return strings.TrimRight(b.String(), "\n")
}

func messageLabel(message ai.Message) string {
	switch message.(type) {
	case ai.SystemMessage:
		return "system"
	case ai.UserMessage:
		return "user"
	case ai.AssistantMessage:
		return "assistant"
	case ai.ToolMessage:
		return "tool"
	default:
		return "unknown"
	}
}
