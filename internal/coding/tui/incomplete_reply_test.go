package tui

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimelineRendersIncompleteReplyAsItsOwnBlock(t *testing.T) {
	t.Parallel()

	state := coding.State{
		Transcript: []ai.Message{ai.UserText("ask"), ai.AssistantText("COMMITTED-REPLY")},
		IncompleteReplies: []coding.IncompleteReply{{
			Turn: 1, Text: "half an answer", Reason: "stream ended early", Bytes: 14,
		}},
	}

	blocks := projectTimeline(state)
	require.Len(t, blocks, 3)

	incomplete := blocks[2]
	assert.Equal(t, blockIncomplete, incomplete.kind)
	assert.Equal(t, "Interrupted reply", incomplete.title)

	rendered := renderTimeline(blocks, newMarkdownRenderer(4), 80, themeDark, true)
	assert.Contains(t, rendered, "Interrupted reply")
	assert.Contains(t, rendered, "stream ended early")
	assert.Contains(t, rendered, "not part of the conversation")
	assert.Contains(t, rendered, "half an answer")
}

// TestTimelineSaysWhenTheRetainedTextWasCutShort keeps the block honest about
// its own bound: Bytes is the size before the emitter truncated the record, so
// a larger Bytes means what is shown is not the whole fragment.
func TestTimelineSaysWhenTheRetainedTextWasCutShort(t *testing.T) {
	t.Parallel()

	render := func(bytes int) string {
		state := coding.State{
			Transcript: []ai.Message{ai.UserText("ask"), ai.AssistantText("COMMITTED-REPLY")},
			IncompleteReplies: []coding.IncompleteReply{{
				Turn: 1, Text: "half an answer", Reason: "stream ended early", Bytes: bytes,
			}},
		}

		return renderTimeline(projectTimeline(state), newMarkdownRenderer(4), 80, themeDark, true)
	}

	assert.Contains(t, render(64<<10), "The retained text was cut short: 14 of 65536 bytes kept")
	assert.NotContains(t, render(len("half an answer")), "cut short",
		"a record that holds everything produced says nothing about truncation")
}

func TestAssistantCopyExcludesIncompleteReply(t *testing.T) {
	t.Parallel()

	model, _ := copyModel(t,
		ai.UserText("ask"),
		ai.AssistantText("COMMITTED-REPLY"),
	)

	state := model.state
	state.IncompleteReplies = []coding.IncompleteReply{{
		Turn: 1, Text: "INCOMPLETE-FRAGMENT", Reason: "stream ended early", Bytes: 19,
	}}
	model.state = state

	responses := model.assistantResponses()
	require.Len(t, responses, 1)
	assert.Equal(t, "COMMITTED-REPLY", responses[0])
	assert.NotContains(t, strings.Join(responses, "\n"), "INCOMPLETE-FRAGMENT")
}
