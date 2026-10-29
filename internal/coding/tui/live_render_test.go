//nolint:wsl_v5 // Each live-render case keeps its fixture and assertion adjacent.
package tui

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLargeMutableSeamUsesTheWorkerAfterProse(t *testing.T) {
	t.Parallel()

	for _, shape := range []string{"open_fence", "list", "table"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()

			renderer := newMarkdownRenderer(4)
			body := "Here is the implementation.\n\n" + liveFrameShapes(32 << 10)[shape]
			previous, err := renderer.renderLiveRows("draft", body, 100, 2, themeDark, false, true)
			require.NoError(t, err)
			before := renderer.uncachedRenders
			grown := grownLiveShape(shape, body, 1)
			rows, err := renderer.renderLiveRows("draft", grown, 100, 2, themeDark, false, true)
			require.NoError(t, err)
			assert.Equal(t, before, renderer.uncachedRenders, "a frozen paragraph cannot hide an expensive seam")
			assert.Equal(t, previous.all(), rows.all())
			miss, ok := renderer.takeLiveMiss()
			require.True(t, ok)
			assert.Equal(t, grown, miss.content)

			worker := newLiveRenderWorker()
			t.Cleanup(worker.shutdown)
			worker.submit(liveRenderRequest{epoch: 1, miss: miss})
			messages := make(chan any, 1)
			go func() { messages <- worker.wait()() }()
			require.Eventually(t, func() bool { return len(messages) > 0 }, 30*time.Second, 10*time.Millisecond)
			ready, ok := (<-messages).(liveRenderReadyMsg)
			require.True(t, ok)
			require.NoError(t, ready.result.err)
			require.True(t, renderer.installExactRows(miss, ready.result.rows, ready.result.cost))
			finalRows, err := renderer.renderLiveRows("draft", grown, 100, 2, themeDark, false, true)
			require.NoError(t, err)
			assert.Equal(t, before, renderer.uncachedRenders, "a paused final frame is installed without synchronous rendering")

			whole, err := newMarkdownRenderer(1).render(grown, 100, themeDark, false)
			require.NoError(t, err)
			assert.Equal(t, insetRowSlice(splitTranscriptRows(whole), 2), finalRows.all())
		})
	}
}

func TestLargeProseKeepsItsCheapIncrementalTail(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := strings.Repeat("A short ordinary paragraph.\n\n", 1500) + "A live tail."
	_, err := renderer.renderLiveRows("draft", body, 100, 2, themeDark, false, true)
	require.NoError(t, err)
	before := renderer.renderedBytes
	_, err = renderer.renderLiveRows("draft", body+" More words.", 100, 2, themeDark, false, true)
	require.NoError(t, err)
	assert.Less(t, renderer.renderedBytes-before, liveFrameBytes)
	_, missing := renderer.takeLiveMiss()
	assert.False(t, missing, "an ordinary short seam stays incremental")
}

func TestNewlyFrozenLargeRegionAlsoUsesTheWorker(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := "A short preface.\n\n" + liveRenderProbeBody()
	_, err := renderer.renderLiveRows("draft", body, 100, 2, themeDark, false, true)
	require.NoError(t, err)
	before := renderer.uncachedRenders
	// The new boundary has a short seam, but extending the frozen prefix would
	// still synchronously render the entire preceding code block.
	grown := body + "```\n\nA short explanation.\n\nThe latest paragraph."
	_, err = renderer.renderLiveRows("draft", grown, 100, 2, themeDark, false, true)
	require.NoError(t, err)
	assert.Equal(t, before, renderer.uncachedRenders)
	miss, ok := renderer.takeLiveMiss()
	require.True(t, ok)
	assert.Equal(t, grown, miss.content)
}

func TestLiveRenderDeliverySurvivesDiscardedResults(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{"geometry", "error", "reset"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()

			model := fullscreenModel(t, stubController{state: readyState()}, true)
			worker := &liveRenderWorker{
				requests: make(chan liveRenderRequest, liveRenderQueue),
				results:  make(chan liveRenderResult, 1),
				done:     make(chan struct{}),
			}
			model.liveRender = worker
			t.Cleanup(worker.shutdown)
			setBody := func(body string) {
				model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: body})
				model.rerenderTranscript(false)
			}
			body := liveRenderProbeBody()
			setBody(body)
			setBody(body + "// first request\n")
			receive := model.armLiveWork()
			require.NotNil(t, receive)
			first := <-worker.requests // The first request is executing.

			switch scenario {
			case "geometry":
				model.width++
				setBody(body + "// first request\n")
			case "reset":
				model.resetScrollback()
				require.True(t, model.liveRenderWait, "reset preserves ownership of the existing receiver")
				setBody(body + "// first request\n")
			}
			final := body + "// first request\nunique_final_delivery := 17\n"
			setBody(final)
			require.Nil(t, model.armLiveWork(), "new work shares the existing receive command")
			latest := <-worker.requests
			require.Greater(t, latest.sequence, first.sequence)
			before := model.markdown.uncachedRenders

			result := liveRenderResult{epoch: first.epoch, sequence: first.sequence, miss: first.miss}
			if scenario == "error" {
				result.err = errors.New("render failed")
			} else {
				require.NotEqual(t, model.liveRenderEpoch, result.epoch)
			}
			worker.results <- result
			_, receive = model.Update(receive())
			require.NotNil(t, receive, "discarding a result keeps submitted work deliverable")
			require.True(t, model.liveRenderWait)
			assert.False(t, model.liveCatchUpWait, "queued work owns catch-up")
			require.Nil(t, model.armLiveWork(), "no second concurrent receiver is armed")

			model.Update(liveCatchUpMsg{generation: model.liveCatchUpGen})
			assert.Equal(t, before, model.markdown.uncachedRenders,
				"a catch-up cannot synchronously compete with outstanding work")
			assert.Empty(t, worker.requests, "rebuilding does not enqueue the same pending version twice")

			rows, err := latest.miss.render(newMarkdownRenderer(1))
			require.NoError(t, err)
			worker.results <- liveRenderResult{
				epoch: latest.epoch, sequence: latest.sequence, miss: latest.miss,
				rows: rows, cost: time.Millisecond,
			}
			_, command := model.Update(receive())
			assert.Nil(t, command, "an exact final version arms neither a receiver nor a timer")
			assert.False(t, model.liveRenderWait)
			assert.False(t, worker.pending())
			assert.Equal(t, before, model.markdown.uncachedRenders)
			assert.Contains(t, strings.Join(model.transcript.flatten(), "\n"), "unique_final_delivery")
		})
	}
}

func TestSettledLiveProjectionRejectsItsOutstandingResult(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	body := liveRenderProbeBody()
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: body})
	model.rerenderTranscript(false)
	epoch := model.liveRenderEpoch
	model.state.Draft = coding.StreamDraft{}
	model.rerenderTranscript(false)
	require.Greater(t, model.liveRenderEpoch, epoch)
	before := model.frameRenders
	model.acceptLiveRender(liveRenderResult{
		epoch: epoch, miss: liveRenderMiss{slot: "draft", content: body}, rows: rowSegments{frozen: []string{"obsolete"}},
	})
	assert.Equal(t, before, model.frameRenders)
	assert.Empty(t, model.markdown.exact, "settling releases live rows permanently")
}

func TestFailedFinalLiveRenderFallsBackWithoutAStuckReceiver(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	worker := &liveRenderWorker{
		requests: make(chan liveRenderRequest, liveRenderQueue),
		results:  make(chan liveRenderResult, 1),
		done:     make(chan struct{}),
	}
	model.liveRender = worker
	body := liveRenderProbeBody()
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: body})
	model.rerenderTranscript(false)
	final := body + "unique_error_recovery := 23\n"
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: final})
	model.rerenderTranscript(false)
	receive := model.armLiveWork()
	require.NotNil(t, receive)
	request := <-worker.requests
	worker.results <- liveRenderResult{
		sequence: request.sequence, epoch: request.epoch, miss: request.miss, err: errors.New("render failed"),
	}
	_, catchUp := model.Update(receive())
	require.NotNil(t, catchUp)
	assert.False(t, model.liveRenderWait)
	assert.False(t, worker.pending())
	require.True(t, model.liveCatchUpWait, "an error with no newer work retains the one-shot fallback")
	_, command := model.Update(liveCatchUpMsg{generation: model.liveCatchUpGen})
	assert.Nil(t, command)
	assert.False(t, model.liveCatchUpWait)
	assert.Contains(t, strings.Join(model.transcript.flatten(), "\n"), "unique_error_recovery")
}

func TestReplacedLiveWorkerCannotRetireTheCurrentReceiver(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	model.liveRender = &liveRenderWorker{done: make(chan struct{})}
	model.liveRenderWait = true
	model.acceptLiveRender(liveRenderResult{worker: &liveRenderWorker{}})
	assert.True(t, model.liveRenderWait, "late delivery belongs to the old worker")
}

// liveRenderProbeBody is a streamed answer whose tail cannot be frozen: an open
// fenced code block. Its render is the cost the event loop must not pay.
func liveRenderProbeBody() string {
	var body strings.Builder
	body.WriteString("```go\n")
	body.WriteString(strings.Repeat("value := compute(alpha, beta)\n", 900))

	return body.String()
}

// TestLiveRenderWorkerProducesTheExactRows pins the worker's ownership and its
// output: a private renderer, immutable input, and rows byte-identical to the
// synchronous path for the same content.
func TestLiveRenderWorkerProducesTheExactRows(t *testing.T) {
	t.Parallel()

	body := liveRenderProbeBody()
	require.GreaterOrEqual(t, len(body), liveFrameBytes)
	grown := body + "// the newest line\n"

	worker := newLiveRenderWorker()
	t.Cleanup(worker.shutdown)

	worker.submit(liveRenderRequest{
		epoch: 7,
		miss: liveRenderMiss{
			slot: "draft", content: grown, width: 100, inset: 2,
			theme: themeDark, noColor: false, window: true,
		},
	})

	message := worker.wait()()
	ready, ok := message.(liveRenderReadyMsg)
	require.True(t, ok, "the worker delivers a result message")
	assert.Equal(t, uint64(7), ready.result.epoch)
	require.NoError(t, ready.result.err)
	assert.Equal(t, grown, ready.result.miss.content)
	assert.Positive(t, ready.result.cost)
	assert.NotEmpty(t, ready.result.rows.all())

	want, err := newMarkdownRenderer(1).renderLiveRows("draft", grown, 100, 2, themeDark, false, true)
	require.NoError(t, err)
	assert.Equal(t, want.all(), ready.result.rows.all(),
		"the background frame is the synchronous frame")
}

// TestLiveRenderWorkerKeepsTheNewestRequest pins the resource bound: one task
// executes and only the newest waiting request survives, so a burst of versions
// cannot queue renders. The newest version is still the one rendered.
func TestLiveRenderWorkerKeepsTheNewestRequest(t *testing.T) {
	t.Parallel()

	worker := &liveRenderWorker{
		requests: make(chan liveRenderRequest, liveRenderQueue),
		results:  make(chan liveRenderResult, 1),
		done:     make(chan struct{}),
	}

	worker.submit(liveRenderRequest{epoch: 1, miss: liveRenderMiss{content: "oldest"}})
	worker.submit(liveRenderRequest{epoch: 2, miss: liveRenderMiss{content: "older"}})
	worker.submit(liveRenderRequest{epoch: 3, miss: liveRenderMiss{content: "newest"}})

	require.Len(t, worker.requests, liveRenderQueue)
	request := <-worker.requests
	assert.Equal(t, uint64(3), request.epoch, "the newest waiting request survives")
	assert.Equal(t, "newest", request.miss.content)
}

// TestLargeLiveTailFrameNeitherRendersNorLosesContent pins the end-to-end frame
// contract: the event loop serves the frame it has, the background result is
// installed, and the transcript then holds the newest content.
func TestLargeLiveTailFrameNeitherRendersNorLosesContent(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	body := liveRenderProbeBody()
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: body})
	model.rerenderTranscript(false)

	rendered := model.markdown.uncachedRenders
	grown := body + "unique_final_line := 42\n"
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: grown})
	model.rerenderTranscript(false)

	assert.Equal(t, rendered, model.markdown.uncachedRenders,
		"the event loop paid for no whole-document render")
	miss, ok := model.markdown.takeLiveMiss()
	require.True(t, ok, "the newest version was handed to the background renderer")
	assert.Equal(t, grown, miss.content)

	rows, err := miss.render(newMarkdownRenderer(1))
	require.NoError(t, err)
	require.True(t, model.markdown.installExactRows(miss, rows, time.Millisecond))
	model.rerenderTranscript(false)

	assert.Contains(t, strings.Join(model.transcript.flatten(), "\n"), "unique_final_line",
		"the installed frame carries the newest content")
	assert.Equal(t, rendered, model.markdown.uncachedRenders,
		"the installed frame is painted without another render")
}

// TestCatchUpRepaintsAStaleFrameWithoutADelta pins the one-shot fallback: when
// the stream pauses after a frame that could not be rendered, the catch-up
// message rebuilds the transcript and the newest rows appear.
func TestCatchUpRepaintsAStaleFrameWithoutADelta(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	body := liveRenderProbeBody()
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: body})
	model.rerenderTranscript(false)

	grown := body + "unique_paused_line := 7\n"
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: grown})
	model.rerenderTranscript(false)
	_, stale := model.markdown.liveCatchUpDelay()
	require.True(t, stale, "a served frame is stale")

	// No further delta arrives, and no background render is outstanding: the
	// catch-up is the only repaint.
	require.False(t, model.liveRenderWait)
	model.Update(liveCatchUpMsg{generation: model.liveCatchUpGen})

	assert.Contains(t, strings.Join(model.transcript.flatten(), "\n"), "unique_paused_line",
		"the paused stream still shows its last line")
	_, stale = model.markdown.liveCatchUpDelay()
	assert.False(t, stale, "the catch-up leaves the slot exact")
}

// TestBackgroundResultRepaintsWithoutADelta pins the normal delivery path: the
// worker's result is installed and repainted with no further delta.
func TestBackgroundResultRepaintsWithoutADelta(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	body := liveRenderProbeBody()
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: body})
	model.rerenderTranscript(false)

	grown := body + "unique_worker_line := 3\n"
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: grown})
	model.rerenderTranscript(false)

	miss, ok := model.markdown.takeLiveMiss()
	require.True(t, ok)
	rows, err := miss.render(newMarkdownRenderer(1))
	require.NoError(t, err)

	model.acceptLiveRender(liveRenderResult{
		epoch: model.liveRenderEpoch, miss: miss, rows: rows, cost: time.Millisecond,
	})

	assert.Contains(t, strings.Join(model.transcript.flatten(), "\n"), "unique_worker_line",
		"the worker's frame is painted without another delta")
	_, stale := model.markdown.liveCatchUpDelay()
	assert.False(t, stale)
}

// TestStaleEpochResultIsDropped pins the invalidation: a result from a replaced
// projection never reaches the new frame.
func TestStaleEpochResultIsDropped(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "short"})
	model.rerenderTranscript(false)

	miss := liveRenderMiss{slot: "draft", content: "stale content", width: 80, theme: themeDark}
	rows, err := miss.render(newMarkdownRenderer(1))
	require.NoError(t, err)

	composes := model.frameRenders
	model.acceptLiveRender(liveRenderResult{
		epoch: model.liveRenderEpoch + 1, miss: miss, rows: rows, cost: time.Millisecond,
	})
	assert.Equal(t, composes, model.frameRenders, "a stale result does not repaint")
}

// TestCatchUpDropsAReplacedProjection pins the invalidation: a catch-up armed for
// one projection cannot repaint another.
func TestCatchUpDropsAReplacedProjection(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	body := liveRenderProbeBody()
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: body})
	model.rerenderTranscript(false)

	generation := model.liveCatchUpGen
	model.resetScrollback()
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "short answer"})

	composes := model.frameRenders
	model.Update(liveCatchUpMsg{generation: generation})

	assert.Equal(t, composes, model.frameRenders,
		"a stale catch-up does not rebuild the replacement's frame")
}

// TestLargeLiveFrameKeepsTheEventLoopResponsive pins the interaction contract: a
// frame that extends a huge un-freezable tail must not pay for the tail on the
// goroutine that also receives keystrokes. The synchronous render of the same
// content is measured as the reference, so the assertion is a ratio and not a
// machine-specific number.
func TestLargeLiveFrameKeepsTheEventLoopResponsive(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	body := liveRenderProbeBody()
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: body})
	model.rerenderTranscript(false)

	grown := body + "value := compute(alpha, beta)\n"
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: grown})
	model.renderDirty = true

	started := time.Now()
	model.Update(renderTickMsg{})
	frameCost := time.Since(started)

	referenceStarted := time.Now()
	if _, err := newMarkdownRenderer(1).renderLiveRows("draft", grown, 100, 2, themeDark, false, true); err != nil {
		t.Fatal(err)
	}
	referenceCost := time.Since(referenceStarted)

	t.Logf("event-loop frame %s, whole render %s", frameCost, referenceCost)
	require.NotZero(t, frameCost)
	assert.Less(t, frameCost, 100*time.Millisecond,
		"the frame that extends a large tail does not render it on the event loop")
	assert.Less(t, frameCost*4, referenceCost,
		"the frame is far cheaper than the render it defers")
}

// TestRowWindowReleasesTheWholeRender pins the residency fix: a window that keeps
// a small share of a record's rows must not keep the record's rendered string
// alive through the rows it retained.
//
//nolint:gosec // G103: compare read-only string addresses to verify backing storage is released; neither pointer is dereferenced.
func TestRowWindowReleasesTheWholeRender(t *testing.T) {
	t.Parallel()

	source := strings.Repeat(strings.Repeat("x", 100)+"\n", 100000)
	base := uintptr(unsafe.Pointer(unsafe.StringData(source)))
	end := base + uintptr(len(source))

	rows := rowSegments{frozen: strings.Split(source, "\n")}.window(99999, 100000)
	require.Len(t, rows.frozen, 1)

	pointer := uintptr(unsafe.Pointer(unsafe.StringData(rows.frozen[0])))
	assert.False(t, pointer >= base && pointer < end,
		"the retained row does not alias the whole render")
	runtime.KeepAlive(source)
	runtime.KeepAlive(rows)
}

// TestRowWindowKeepsProportionalWindowsZeroCopy pins the other side: a window
// that keeps most of the record is not copied.
//
//nolint:gosec // G103: compare read-only string addresses to verify zero-copy windows; neither pointer is dereferenced.
func TestRowWindowKeepsProportionalWindowsZeroCopy(t *testing.T) {
	t.Parallel()

	source := strings.Repeat("row\n", 1000)
	rows := rowSegments{frozen: strings.Split(source, "\n")}.window(0, 900)
	require.NotEmpty(t, rows.frozen)

	base := uintptr(unsafe.Pointer(unsafe.StringData(source)))
	end := base + uintptr(len(source))
	pointer := uintptr(unsafe.Pointer(unsafe.StringData(rows.frozen[0])))
	assert.True(t, pointer >= base && pointer < end,
		"a proportional window keeps the render's own rows")
	runtime.KeepAlive(source)
	runtime.KeepAlive(rows)
}

// TestPendingBufferReleasesItsEvents pins the buffer release: a flush or a drop
// must not keep the drained payloads reachable through the slice's backing array.
func TestPendingBufferReleasesItsEvents(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.pending = []streamItem{{event: streamTestDelta(4)}}
	backing := model.pending[:1]
	model.dropPending()

	assert.Zero(t, backing[0].event.Sequence)
	assert.Nil(t, backing[0].event.Payload)

	model.pending = []streamItem{{event: streamTestDelta(5)}}
	backing = model.pending[:1]
	model.flushPending()

	assert.Zero(t, backing[0].event.Sequence)
	assert.Nil(t, backing[0].event.Payload)
}
