package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamingCodeBlockKeepsOneManagedTailAndCommitsEveryRowOnce(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 48, Height: 14})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true

	var source strings.Builder

	source.WriteString("```python\nvalue_00 = 0\npartial")
	model.state.Draft = []coding.MessageDelta{{Kind: ai.StreamTextDelta, Text: source.String()}}
	outputs := []string{commandOutput(model.commitStableTimeline())}
	managedHeight := lipgloss.Height(model.View().Content)

	for index := 1; index < 20; index++ {
		delta := fmt.Sprintf("_%02d\nvalue_%02d = %d", index, index, index)
		source.WriteString(delta)
		model.state.Draft = append(model.state.Draft, coding.MessageDelta{
			Kind: ai.StreamTextDelta,
			Text: delta,
		})
		outputs = append(outputs, commandOutput(model.commitStableTimeline()))
		assert.Equal(
			t,
			managedHeight,
			lipgloss.Height(model.View().Content),
			"delta %d expanded the managed inline frame",
			index,
		)
	}

	source.WriteString("\n```")
	model.state.Transcript = []ai.Message{ai.AssistantText(source.String())}
	model.state.Draft = nil
	outputs = append(outputs, commandOutput(model.commitStableTimeline()))

	combined := ansi.Strip(strings.Join(outputs, "\n"))

	for index := range 20 {
		value := fmt.Sprintf("value_%02d", index)
		assert.Equalf(t, 1, strings.Count(combined, value), "row %q", value)
	}
}

func TestStreamingTableStaysMutableUntilFinalized(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 48, Height: 14})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true

	source := "| name | result |\n"
	model.state.Draft = []coding.MessageDelta{{Kind: ai.StreamTextDelta, Text: source}}
	first := commandOutput(model.commitStableTimeline())
	assert.NotContains(t, ansi.Strip(first), "name")
	assert.Equal(t, "Table · preparing…", ansi.Strip(model.streaming.tail))

	managedHeight := lipgloss.Height(model.View().Content)

	delta := "| --- | --- |\n| alpha"
	source += delta
	model.state.Draft = append(model.state.Draft, coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: delta,
	})
	second := commandOutput(model.commitStableTimeline())
	assert.NotContains(t, ansi.Strip(second), "alpha")
	assert.Equal(t, "Table · 0 rows", ansi.Strip(model.streaming.tail))
	assert.Equal(t, managedHeight, lipgloss.Height(model.View().Content))

	delta = " | one |\n| beta"
	source += delta
	model.state.Draft = append(model.state.Draft, coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: delta,
	})
	third := commandOutput(model.commitStableTimeline())
	assert.NotContains(t, ansi.Strip(third), "alpha")
	assert.Equal(t, "Table · 1 row · alpha | one", ansi.Strip(model.streaming.tail))
	assert.Equal(t, managedHeight, lipgloss.Height(model.View().Content))

	delta = " | two"
	source += delta
	model.state.Draft = append(model.state.Draft, coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: delta,
	})
	fourth := commandOutput(model.commitStableTimeline())
	assert.Empty(t, fourth)
	assert.Equal(
		t,
		"Table · 1 row · alpha | one",
		ansi.Strip(model.streaming.tail),
		"an incomplete row must not change the table preview",
	)
	assert.Equal(t, managedHeight, lipgloss.Height(model.View().Content))

	delta = " |\n"
	source += delta
	model.state.Draft = append(model.state.Draft, coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: delta,
	})
	fifth := commandOutput(model.commitStableTimeline())
	assert.NotContains(t, ansi.Strip(fifth), "beta")
	assert.Equal(t, "Table · 2 rows · beta | two", ansi.Strip(model.streaming.tail))
	assert.Equal(t, managedHeight, lipgloss.Height(model.View().Content))

	model.state.Transcript = []ai.Message{ai.AssistantText(source)}
	model.state.Draft = nil
	final := commandOutput(model.commitStableTimeline())
	combined := ansi.Strip(strings.Join(
		[]string{first, second, third, fourth, fifth, final},
		"\n",
	))

	for _, value := range []string{"alpha", "beta", "one", "two"} {
		assert.Equalf(t, 1, strings.Count(combined, value), "cell %q", value)
	}

	assert.NotContains(t, combined, "Table ·")
}

func TestStreamingLongTableKeepsFixedPreviewHeight(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 48, Height: 14})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true

	var source strings.Builder

	source.WriteString("| name | result |\n| --- | --- |\n")
	model.state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta,
		Text: source.String(),
	}}
	assert.Empty(t, commandOutput(model.commitStableTimeline()))
	managedHeight := lipgloss.Height(model.View().Content)

	for index := 1; index <= 40; index++ {
		delta := fmt.Sprintf("| row_%02d | value_%02d |\n", index, index)
		source.WriteString(delta)
		model.state.Draft = append(model.state.Draft, coding.MessageDelta{
			Kind: ai.StreamTextDelta,
			Text: delta,
		})

		assert.Empty(t, commandOutput(model.commitStableTimeline()))
		assert.Contains(t, ansi.Strip(model.streaming.tail), fmt.Sprintf("Table · %d ", index))
		assert.Equal(t, managedHeight, lipgloss.Height(model.View().Content))
	}

	model.state.Transcript = []ai.Message{ai.AssistantText(source.String())}
	model.state.Draft = nil
	final := ansi.Strip(commandOutput(model.commitStableTimeline()))
	assert.NotContains(t, final, "Table ·")

	for index := 1; index <= 40; index++ {
		assert.Equal(t, 1, strings.Count(final, fmt.Sprintf("row_%02d", index)))
		assert.Equal(t, 1, strings.Count(final, fmt.Sprintf("value_%02d", index)))
	}
}

func TestInterruptedStreamingDraftFlushesUncommittedTail(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 48, Height: 14})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true
	model.state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta,
		Text: "completed row\nunfinished tail",
	}}

	first := commandOutput(model.commitStableTimeline())
	assert.NotContains(t, ansi.Strip(first), "unfinished tail")
	assert.Contains(t, ansi.Strip(model.View().Content), "unfinished tail")

	model.state.Draft = nil
	final := commandOutput(model.commitStableTimeline())
	combined := ansi.Strip(first + "\n" + final)
	assert.Equal(t, 1, strings.Count(combined, "completed row"))
	assert.Equal(t, 1, strings.Count(combined, "unfinished tail"))
	assert.False(t, model.streaming.active)
}

func TestStreamingWaitsForMatchingDurableAssistantPromotion(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 48, Height: 14})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true

	source := "```text\nfirst row\nmiddle row\nsecond row"
	promoted, _ := model.syncStreamingDraft("run-1:1", source)
	require.NotEmpty(t, promoted)

	model.state.Transcript = []ai.Message{ai.AssistantText(source)}
	model.state.MessageCandidates = []coding.CandidateIdentity{{RunID: "run-1", Turn: 1}}
	model.scrollback.messages = 0

	writes := model.streamingScrollbackWrites(nil)
	assert.Empty(t, writes)
	assert.True(t, model.streaming.active)
	assert.True(t, model.hasPendingStreamingAssistant())

	active := ansi.Strip(model.renderTimelineBlocks(model.activeTimelineBlocks()))
	assert.NotContains(t, active, "first row")
	assert.Contains(t, active, "second row")

	stable := []timelineBlock{{kind: blockAssistant, id: "run-1:1", body: source}}
	finalWrites := model.streamingScrollbackWrites(stable)

	finalParts := make([]string, len(finalWrites))
	for index := range finalWrites {
		finalParts[index] = finalWrites[index].content
	}

	final := strings.Join(finalParts, "\n")
	combined := ansi.Strip(promoted + "\n" + final)
	assert.Equal(t, 1, strings.Count(combined, "first row"))
	assert.Equal(t, 1, strings.Count(combined, "middle row"))
	assert.Equal(t, 1, strings.Count(combined, "second row"))
	assert.False(t, model.streaming.active)
}

func TestPlanStreamingCandidateIsRetractableAndCommitsExactlyOnce(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 48, Height: 14})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction = coding.InteractionState{Active: true, Mode: coding.ModePlan}
	model.state.DraftCandidate = coding.CandidateIdentity{RunID: "plan-run", Turn: 1}
	model.state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta, Text: "provisional Plan that must remain retractable",
	}}

	writes := model.streamingScrollbackWrites(nil)
	assert.Empty(t, writes)
	assert.True(t, model.streaming.active)
	assert.True(t, model.streaming.retractable)
	assert.Zero(t, model.streaming.emitted)

	model.state.Draft = nil
	model.state.DraftCandidate = coding.CandidateIdentity{}
	writes = model.streamingScrollbackWrites(nil)
	assert.Empty(t, writes)
	assert.False(t, model.streaming.active)

	const accepted = "# Accepted Plan\n\nOne authoritative block."

	model.state.DraftCandidate = coding.CandidateIdentity{RunID: "plan-run", Turn: 2}
	model.state.Draft = []coding.MessageDelta{{Kind: ai.StreamTextDelta, Text: accepted}}
	assert.Empty(t, model.streamingScrollbackWrites(nil))
	model.state.Draft = nil
	model.state.DraftCandidate = coding.CandidateIdentity{}
	stable := []timelineBlock{{kind: blockAssistant, id: "plan-run:2", body: accepted}}
	writes = model.streamingScrollbackWrites(stable)

	var rendered strings.Builder
	for _, write := range writes {
		rendered.WriteString(write.content)
	}

	output := ansi.Strip(rendered.String())
	assert.Equal(t, 1, strings.Count(output, "Accepted Plan"))
	assert.Equal(t, 1, strings.Count(output, "One authoritative block"))
	assert.False(t, model.streaming.active)
}

func TestStreamingKeepsRenderCoordinatesAcrossResize(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 24, Height: 12})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true

	source := "```text\nresize_row_0\nresize_row_1\npartial"
	model.state.Draft = []coding.MessageDelta{{Kind: ai.StreamTextDelta, Text: source}}
	first := commandOutput(model.commitStableTimeline())
	assert.Equal(t, 24, model.streaming.width)

	model.Update(tea.WindowSizeMsg{Width: 80, Height: 20})

	delta := "_tail\nresize_row_2\n```"
	source += delta
	model.state.Draft = append(model.state.Draft, coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: delta,
	})
	second := commandOutput(model.commitStableTimeline())
	assert.Equal(t, 24, model.streaming.width)

	model.state.Transcript = []ai.Message{ai.AssistantText(source)}
	model.state.Draft = nil
	final := commandOutput(model.commitStableTimeline())
	combined := ansi.Strip(first + "\n" + second + "\n" + final)

	for _, row := range []string{"resize_row_0", "resize_row_1", "resize_row_2"} {
		assert.Equalf(t, 1, strings.Count(combined, row), "row %q", row)
	}
}

func TestStableStreamPromotionWaitsOnlyForManagedGeometryChanges(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 48, Height: 14})
	model.scrollbackOutput = false
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true
	model.state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta,
		Text: "first paragraph\n\nsecond paragraph\n\npartial",
	}}

	first := sequenceMessages(t, model.commitStableTimeline())
	_, firstWaits := first[0].(scrollbackRenderReadyMsg)
	assert.True(t, firstWaits, "the first tail row changes managed geometry")

	model.state.Draft = append(model.state.Draft, coding.MessageDelta{
		Kind: ai.StreamTextDelta,
		Text: " tail\n\nthird paragraph\n\nnext partial",
	})
	second := sequenceMessages(t, model.commitStableTimeline())
	_, secondWaits := second[0].(scrollbackRenderReadyMsg)
	assert.False(t, secondWaits, "fixed-height stream promotion must not add a frame delay")
}

func TestActiveMarkdownTableStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		start  int
		active bool
	}{
		{
			name:   "pending header",
			source: "intro\n\nname | value\n",
			start:  strings.Index("intro\n\nname | value\n", "name"),
			active: true,
		},
		{
			name:   "confirmed narrow delimiter",
			source: "intro\n\n| name | value |\n|-|-|\n| alpha | one |\n",
			start:  strings.Index("intro\n\n| name | value |\n|-|-|\n| alpha | one |\n", "| name"),
			active: true,
		},
		{
			name:   "closed table",
			source: "| name | value |\n|-|-|\n| alpha | one |\n\nafter\n",
			active: false,
		},
		{
			name:   "pipe rows inside fence",
			source: "```text\nname | value\n-|-\n```\n",
			active: false,
		},
		{
			name:   "fence marker with trailing text does not close",
			source: "```text\n````not a close\nname | value\n-|-\n```\n",
			active: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			start, active := activeMarkdownTableStart(test.source)
			assert.Equal(t, test.active, active)
			assert.Equal(t, test.start, start)
		})
	}
}
