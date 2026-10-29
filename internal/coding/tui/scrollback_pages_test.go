package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scrollbackTestModel is an inline model with an empty presentation queue, so a
// test measures only the writes it creates.
func scrollbackTestModel(t *testing.T) *Model {
	t.Helper()

	model := readyModel(t, true)
	model.presentation = presentationState{}
	model.scrollbackOutput = false

	return model
}

// scrollbackLines builds lines wide enough to force several pages.
func scrollbackLines(count int) []string {
	lines := make([]string, 0, count)
	for index := range count {
		lines = append(lines, fmt.Sprintf("line-%05d-%s", index, strings.Repeat("x", 180)))
	}

	return lines
}

// drainScrollbackSources renders every page a source list produces, so a test
// can assert the content the eager string path used to return in one write.
func drainScrollbackSources(model *Model, sources []scrollbackSource) []scrollbackWrite {
	var writes []scrollbackWrite

	for _, source := range sources {
		for {
			write, ok := source.nextPage(model, scrollbackPageBytes)
			if !ok {
				break
			}

			writes = append(writes, write)
		}
	}

	return writes
}

func TestSplitScrollbackPagesKeepsEveryLineWhole(t *testing.T) {
	t.Parallel()

	lines := scrollbackLines(5000)
	content := strings.Join(lines, "\n")
	pages := splitScrollbackPages(content, scrollbackPageBytes)
	require.Greater(t, len(pages), 1)

	rejoined := make([]string, 0, len(lines))
	for index, page := range pages {
		assert.LessOrEqual(t, len(page), scrollbackPageBytes, "page %d exceeds the byte budget", index)
		rejoined = append(rejoined, strings.Split(page, "\n")...)
	}

	assert.Equal(t, lines, rejoined, "every line is preserved exactly once and in order")

	oversized := strings.Repeat("y", scrollbackPageBytes+64)
	require.Equal(t, []string{oversized}, splitScrollbackPages(oversized, scrollbackPageBytes),
		"an unbreakable line stays whole so ANSI sequences are never cut")
}

func TestPrintScrollbackPagesWrapInOrderAndDrainTheQueue(t *testing.T) {
	t.Parallel()

	model := scrollbackTestModel(t)
	lines := scrollbackLines(3000)
	content := strings.Join(lines, "\n")

	command := model.printScrollback(content)
	require.NotNil(t, command)
	require.Greater(t, len(model.presentation.writes), 1, "a large payload is paged")

	for index, write := range model.presentation.writes {
		assert.LessOrEqual(t, len(write.content), scrollbackPageBytes,
			"write %d exceeds the byte budget", index)
	}

	outputs := modelCommandOutputs(model, command)
	require.Greater(t, len(outputs), 1)
	assert.Equal(t, content, strings.Join(outputs, "\n"),
		"the pages reconstruct the logical payload exactly")
	assert.Empty(t, model.presentation.writes)
	assert.Zero(t, model.presentation.queuedBytes)
}

// TestScrollbackBackpressureHoldsTheStableProjection pins the slow-terminal
// contract: while the queue is full the stable cursor does not advance, and the
// content is printed once the terminal catches up. Nothing is dropped.
func TestScrollbackBackpressureHoldsTheStableProjection(t *testing.T) {
	t.Parallel()

	model := scrollbackTestModel(t)
	model.state = readyState()
	model.state.Transcript = []ai.Message{ai.UserText("STABLE-MARKER")}
	model.state.MessageCandidates = []coding.CandidateIdentity{{}}

	filler := strings.Join(scrollbackLines(26000), "\n")
	require.Greater(t, len(filler), scrollbackQueueBytes)
	command := model.printScrollback(filler)
	require.True(t, model.presentation.backpressured())
	require.NotEmpty(t, model.presentation.pendingSources,
		"content beyond the cap waits as a bounded source")

	before := model.scrollback.messages
	assert.Nil(t, model.commitStableTimeline(), "a full queue does not produce more pages")
	assert.Equal(t, before, model.scrollback.messages, "the stable cursor stays put")

	printed := strings.Join(modelCommandOutputs(model, command), "\n")
	assert.Contains(t, printed, "STABLE-MARKER", "catch-up prints the held stable content")
	assert.Greater(t, model.scrollback.messages, before, "the cursor advanced after the queue drained")
	assert.Empty(t, model.presentation.writes)
	assert.False(t, model.presentation.hasPendingScrollback())
}

func TestScrollbackRouteWaitsForEveryPage(t *testing.T) {
	t.Parallel()

	model := scrollbackTestModel(t)
	filler := strings.Join(scrollbackLines(3000), "\n")
	command := model.printScrollback(filler)
	require.Greater(t, len(model.presentation.writes), 1)

	model.openToolDetailRoute(toolDetailView{title: "route"})
	assert.Equal(t, routeNone, model.route.kind, "the route waits for the queued pages")

	modelCommandOutputs(model, command)
	assert.Equal(t, routeToolDetail, model.route.kind)
}

func TestScrollbackSessionResetDropsDeferredPagesAndKeepsAcceptedWrites(t *testing.T) {
	t.Parallel()

	model := scrollbackTestModel(t)
	filler := strings.Join(scrollbackLines(26000), "\n")
	command := model.printScrollback(filler)
	require.NotNil(t, command)
	require.NotEmpty(t, model.presentation.pendingSources)
	accepted := len(model.presentation.writes)

	model.resetScrollback()

	assert.Empty(t, model.presentation.pendingSources)
	assert.Len(t, model.presentation.writes, accepted, "accepted output is still on its way")
}

// TestStartupHistoryPrintsInPages pins the inline startup seam: the banner and
// the loaded transcript reach the terminal as byte-bounded pages, in order, and
// without one joined history string.
func TestStartupHistoryPrintsInPages(t *testing.T) {
	t.Parallel()

	model := scrollbackTestModel(t)

	state := readyState()
	for index := range 3000 {
		state.Transcript = append(state.Transcript,
			ai.UserText(fmt.Sprintf("history-%05d-%s", index, strings.Repeat("h", 100))))
	}

	model.state = state

	command := model.commitBannerOutput("STARTUP-BANNER")
	require.NotNil(t, command)
	require.Greater(t, len(model.presentation.writes), 1, "the startup history is paged")

	for index, write := range model.presentation.writes {
		assert.LessOrEqual(t, len(write.content), scrollbackPageBytes,
			"startup page %d exceeds the byte budget", index)
	}

	outputs := modelCommandOutputs(model, command)
	printed := strings.Join(outputs, "\n")
	assert.Contains(t, printed, "STARTUP-BANNER")
	require.Less(t, strings.Index(printed, "STARTUP-BANNER"),
		strings.Index(printed, "history-00000"))

	for index := 1; index < 3000; index++ {
		assert.Less(t,
			strings.Index(printed, fmt.Sprintf("history-%05d", index-1)),
			strings.Index(printed, fmt.Sprintf("history-%05d", index)),
			"history row %d is out of order", index,
		)
	}
}

// TestScrollbackPageContinuationMatchesOnePayload pins the separator contract
// across a page boundary: a continuation page adds no blank row, and a new
// logical block keeps its one-row conversation gap.
func TestScrollbackPageContinuationMatchesOnePayload(t *testing.T) {
	t.Parallel()

	model := scrollbackTestModel(t)
	first := model.printScrollback("FIRST-BLOCK")
	require.NotNil(t, first)

	body := strings.Join(scrollbackLines(3000), "\n")
	require.Nil(t, model.printScrollback(body), "the previous write is still in flight")
	require.Greater(t, len(model.presentation.writes), 2)

	assert.True(t, strings.HasPrefix(model.presentation.writes[1].content, "\n"),
		"a new logical block opens with the conversation gap")
	assert.False(t, strings.HasPrefix(model.presentation.writes[2].content, "\n"),
		"a continuation page relies on the terminal newline")

	outputs := modelCommandOutputs(model, first)
	require.Greater(t, len(outputs), 2)
	assert.Equal(t, "FIRST-BLOCK", outputs[0])
	assert.Equal(t, "\n"+body, strings.Join(outputs[1:], "\n"),
		"the second logical block keeps one blank row and every body line")
}

// hugeScrollbackPayload builds one payload far above the write queue's byte cap
// without keeping a line slice for it.
func hugeScrollbackPayload(lines int) string {
	filler := strings.Repeat("x", 180)

	var content strings.Builder
	content.Grow(lines * 193)

	for index := range lines {
		if index > 0 {
			content.WriteByte('\n')
		}

		fmt.Fprintf(&content, "huge-%06d-%s", index, filler)
	}

	return content.String()
}

// TestScrollbackDeferredWriteHoldsOnePageInsteadOfThePayload pins the queue's
// byte cap. A single logical write larger than the cap waits as a cursor over
// the caller's string, so the deferred content costs one page rather than a
// rendered copy per page; before this change every page stayed in
// presentation.pendingPages and the payload was retained a second time.
//
// The caller's payload is built before the measurement, so the guard covers what
// the write queue itself retains.
//
//nolint:paralleltest // Heap residency comparisons must not overlap other tests.
func TestScrollbackDeferredWriteHoldsOnePageInsteadOfThePayload(t *testing.T) {
	model := scrollbackTestModel(t)
	payload := hugeScrollbackPayload(600000)
	require.Greater(t, len(payload), 100<<20)

	var command tea.Cmd

	resident := retainedHeap(func() any {
		command = model.printScrollback(payload)

		return nil
	})

	require.NotNil(t, command)
	require.True(t, model.presentation.backpressured())
	require.Len(t, model.presentation.pendingSources, 1, "the payload waits as one source")

	pager, ok := model.presentation.pendingSources[0].(*scrollbackTextPager)
	require.True(t, ok, "a rendered string waits as a cursor over it, not as page copies")
	assert.Equal(t, pager.content, payload, "the pager references the caller's string")
	assert.LessOrEqual(t, model.presentation.queuedBytes, scrollbackQueueBytes+scrollbackPageBytes,
		"the rendered queue holds its cap plus at most one page")
	assert.Less(t, resident, uint64(8<<20),
		"the deferred write retains its cap, not a second copy of the payload")

	// The eager split is what the deferred queue used to hold: one rendered copy
	// per page. It is measured here rather than asserted from memory, so the two
	// shapes are compared on the same payload.
	eager := retainedHeap(func() any {
		return splitScrollbackPages(payload, scrollbackPageBytes)
	})
	t.Logf("payload=%dMB queue=%dKB retained=%dKB eager=%dMB", len(payload)>>20,
		model.presentation.queuedBytes>>10, resident>>10, eager>>20)

	assert.Greater(t, eager, uint64(64<<20), "the eager split holds a second copy of the payload")
	assert.Less(t, resident*8, eager, "the lazy source retains a fraction of the eager split")
}

// TestScrollbackTextPagerSplitsAtRowBoundaries pins the page boundary the lazy
// text path shares with the eager split: a row that would exceed the budget
// starts the next page, a trailing newline keeps its empty last row, and an
// unbreakable row stays whole.
func TestScrollbackTextPagerSplitsAtRowBoundaries(t *testing.T) {
	t.Parallel()

	model := scrollbackTestModel(t)

	pager := newScrollbackTextPager("aaa\nbbb\nccc", false)

	first, ok := pager.nextPage(model, 7)
	require.True(t, ok)
	assert.Equal(t, "aaa\nbbb", first.content, "the page stops before the row that would exceed it")
	assert.False(t, first.continuation, "the first page of a source opens it")

	second, ok := pager.nextPage(model, 7)
	require.True(t, ok)
	assert.Equal(t, "ccc", second.content)
	assert.True(t, second.continuation, "later pages continue the previous write")

	_, ok = pager.nextPage(model, 7)
	assert.False(t, ok)

	trailing := newScrollbackTextPager("aaa\n", false)
	page, ok := trailing.nextPage(model, 64)
	require.True(t, ok)
	assert.Equal(t, "aaa\n", page.content, "a trailing newline keeps its empty last row")

	_, ok = trailing.nextPage(model, 64)
	assert.False(t, ok)

	oversized := strings.Repeat("y", scrollbackPageBytes+64)
	wide := newScrollbackTextPager(oversized, true)
	page, ok = wide.nextPage(model, scrollbackPageBytes)
	require.True(t, ok)
	assert.Equal(t, oversized, page.content, "an unbreakable row stays whole")
	assert.True(t, page.continuation, "the caller's continuation is reported on the first page")

	_, ok = wide.nextPage(model, scrollbackPageBytes)
	assert.False(t, ok)

	_, ok = newScrollbackTextPager("", false).nextPage(model, 64)
	assert.False(t, ok)
}

// TestScrollbackDeferredWriteKeepsOrderAndLosesNothing pins what a lazy source
// must preserve: every row reaches the terminal once and in order, and output
// that arrives while the queue is full waits behind the deferred write instead
// of overtaking it.
func TestScrollbackDeferredWriteKeepsOrderAndLosesNothing(t *testing.T) {
	t.Parallel()

	model := scrollbackTestModel(t)
	payload := strings.Join(scrollbackLines(26000), "\n")
	require.Greater(t, len(payload), scrollbackQueueBytes)

	command := model.printScrollback(payload)
	require.NotNil(t, command)
	require.True(t, model.presentation.backpressured())

	assert.Nil(t, model.printScrollback("TAIL-BLOCK"), "later output waits for the queue")
	require.Len(t, model.presentation.pendingSources, 2, "the tail block queues behind the payload")

	printed := strings.Join(modelCommandOutputs(model, command), "\n")
	assert.Contains(t, printed, payload+"\n\nTAIL-BLOCK",
		"the payload precedes the later block and keeps its one-row gap")
	assert.Equal(t, 26000, strings.Count(printed, "line-"), "every row is printed exactly once")
	assert.False(t, model.presentation.hasPendingScrollback())
}

// TestStableDeltaHandsOverBlocksInsteadOfAJoinedString pins the second half of
// the bound: the inline commit path pages the delta from its blocks, so the
// delta's whole rendering never exists as one string before it is split.
func TestStableDeltaHandsOverBlocksInsteadOfAJoinedString(t *testing.T) {
	t.Parallel()

	model := scrollbackTestModel(t)

	state := readyState()
	for index := range 2000 {
		state.Transcript = append(state.Transcript,
			ai.UserText(fmt.Sprintf("delta-%05d-%s", index, strings.Repeat("d", 200))))
	}

	model.state = state

	sources := model.streamingScrollbackSources(model.takeStableTimelineBlocks())
	require.Len(t, sources, 1)

	pager, ok := sources[0].(*scrollbackPager)
	require.True(t, ok, "the stable delta is paged from its blocks")
	assert.False(t, pager.empty())

	command := model.printScrollbackSources(sources)
	require.NotNil(t, command)

	printed := strings.Join(modelCommandOutputs(model, command), "\n")
	for index := range 2000 {
		assert.Contains(t, printed, fmt.Sprintf("delta-%05d", index), "row %d is printed", index)
	}

	assert.False(t, model.presentation.hasPendingScrollback())
}
