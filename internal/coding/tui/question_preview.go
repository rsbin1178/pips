package tui

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/question"
)

// questionPreview is the local card shown as soon as the user answers or
// cancels, before the durable Tool result reaches the transcript projection.
// Once that result is present the preview is dropped, so the conversation holds
// exactly one card in every presentation mode.
type questionPreview struct {
	request    question.Request
	resolution question.Resolution
	rejected   bool
	// position is the transcript position the durable Tool result will occupy.
	// It keeps a preview above later assistant messages when another question
	// was asked in the meantime.
	position int
	// order breaks ties between questions answered before their results land.
	order uint64
}

func (m *Model) previewQuestionOutcome(
	request question.Request,
	resolution question.Resolution,
	rejected bool,
) {
	if request.ToolCallID == "" {
		return
	}

	if m.questionPreviews == nil {
		m.questionPreviews = make(map[string]questionPreview)
	}

	m.questionPreviewSeq++
	m.questionPreviews[request.ToolCallID] = questionPreview{
		request:    question.CloneRequest(request),
		resolution: question.CloneResolution(resolution),
		rejected:   rejected,
		position:   len(m.state.Transcript) + 1,
		order:      m.questionPreviewSeq,
	}
	// The transcript window is owned by the model, and no Runtime event has
	// arrived yet to trigger its normal render path. Repaint it now so the card
	// is in the message area in the same frame that closes the prompt.
	m.renderTranscript(true)
}

// optimisticQuestionBlocks appends the local cards that stand in for Tool
// results the Runtime has not published yet. A preview is removed as soon as the
// durable result is in the transcript, which is also what keeps the later card
// from being rendered twice.
func (m *Model) optimisticQuestionBlocks() []timelineBlock {
	if len(m.questionPreviews) == 0 {
		return nil
	}

	projected := m.questionResultIDs()

	type previewEntry struct {
		block timelineBlock
		order uint64
	}

	entries := make([]previewEntry, 0, len(m.questionPreviews))
	for toolCallID, preview := range m.questionPreviews {
		if _, ok := projected[toolCallID]; ok {
			delete(m.questionPreviews, toolCallID)

			continue
		}

		block, ok := projectQuestionPreview(preview)
		if !ok {
			continue
		}

		block.position = preview.position
		entries = append(entries, previewEntry{block: block, order: preview.order})
	}

	slices.SortFunc(entries, func(left, right previewEntry) int {
		switch {
		case left.block.position < right.block.position:
			return -1
		case left.block.position > right.block.position:
			return 1
		case left.order < right.order:
			return -1
		case left.order > right.order:
			return 1
		default:
			return strings.Compare(left.block.id, right.block.id)
		}
	})

	blocks := make([]timelineBlock, 0, len(entries))
	for _, entry := range entries {
		blocks = append(blocks, entry.block)
	}

	return blocks
}

// insertQuestionPreviews places the local answer cards before the next turn's
// live output. The preview belongs to the conversation prefix; appending it
// after the streaming draft would paint the model's next answer above it until
// the durable Tool result replaced the preview.
func insertQuestionPreviews(blocks, previews []timelineBlock) []timelineBlock {
	if len(previews) == 0 {
		return blocks
	}

	result := slices.Clone(blocks)
	// Insert from the back so same-position previews keep the order
	// optimisticQuestionBlocks returned, and each preview lands before any
	// block at or after its transcript position.
	for _, preview := range slices.Backward(previews) {
		insertAt := len(result)
		for position := range result {
			if questionPreviewPrecedes(result[position], preview) {
				insertAt = position

				break
			}
		}

		result = slices.Insert(result, insertAt, preview)
	}

	return result
}

// questionPreviewPrecedes reports whether a preview must be inserted before the
// block: the first unsettled block or the first block at or after its position.
func questionPreviewPrecedes(block, preview timelineBlock) bool {
	return blockIsUnsettled(block) || block.position >= preview.position
}

// rebaseQuestionPreviews moves preview positions into a projection window that
// starts at messageStart. Preview positions are absolute transcript positions;
// an inline delta's blocks are projected from a slice, so their positions are
// window-relative. A preview that belongs before the window's first message is
// clamped to its front.
func rebaseQuestionPreviews(previews []timelineBlock, messageStart int) []timelineBlock {
	if len(previews) == 0 {
		return nil
	}

	rebased := make([]timelineBlock, 0, len(previews))
	for _, preview := range previews {
		preview.position = max(1, preview.position-max(0, messageStart))
		rebased = append(rebased, preview)
	}

	return rebased
}

// takeOptimisticQuestionBlocks consumes the preview cards that are about to be
// printed into terminal-native history. Marking the Tool call committed keeps
// the later durable card from being printed a second time in inline mode.
func (m *Model) takeOptimisticQuestionBlocks() []timelineBlock {
	blocks := m.optimisticQuestionBlocks()
	for _, block := range blocks {
		m.markToolActivitiesCommitted(block)

		if len(block.tools) > 0 {
			delete(m.questionPreviews, block.tools[0].id)
		}
	}

	return blocks
}

func (m *Model) questionResultIDs() map[string]struct{} {
	ids := make(map[string]struct{})

	for _, message := range m.state.Transcript {
		for _, part := range portableMessageParts(message) {
			result, ok := part.(ai.ToolResultPart)
			if !ok {
				continue
			}

			ids[result.ToolCallID] = struct{}{}
		}
	}

	return ids
}

func projectQuestionPreview(preview questionPreview) (timelineBlock, bool) {
	request := preview.request
	activity := toolActivity{
		id: request.ToolCallID, name: question.ToolName,
		state: toolStateSucceeded, settled: true,
	}

	if request.Kind == question.RequestFreeform {
		arguments, err := json.Marshal(struct {
			Question string `json:"question"`
		}{Question: request.Prompt})
		if err != nil {
			return timelineBlock{}, false
		}

		activity.arguments = arguments
	} else {
		arguments, err := json.Marshal(question.Spec{Questions: request.Questions})
		if err != nil {
			return timelineBlock{}, false
		}

		activity.arguments = arguments
	}

	if preview.rejected {
		activity.state = toolStateFailed
		activity.result = question.RejectionToolResult
	} else {
		result, err := json.Marshal(struct {
			Answers []question.Answer `json:"answers,omitempty"`
			Chat    string            `json:"chat,omitempty"`
		}{Answers: preview.resolution.Answers, Chat: preview.resolution.Chat})
		if err != nil {
			return timelineBlock{}, false
		}

		activity.result = string(result)
	}

	return projectQuestionToolActivity(&activity)
}
