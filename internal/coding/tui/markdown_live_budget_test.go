//nolint:wsl_v5 // The bound's cases and their measurements stay adjacent.
package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveFrameProbeBody is a block large enough to be worth reusing and shaped so a
// streamed prefix of it cannot be frozen: an open code fence, which is exactly
// the case that makes a live frame's cost grow with the tail.
func liveFrameProbeBody() string {
	var body strings.Builder
	body.WriteString("```go\n")
	for line := range 700 {
		fmt.Fprintf(&body, "value%d := %d\n", line, line)
	}

	return body.String()
}

// TestLargeLiveTailIsHandedToTheBackgroundRenderer pins the bound on what one
// un-freezable live tail may cost per frame: above liveFrameBytes the event loop
// serves the frame it already has and requests the current version, so a
// streaming block cannot own the event loop.
func TestLargeLiveTailIsHandedToTheBackgroundRenderer(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := liveFrameProbeBody()
	require.GreaterOrEqual(t, len(body), liveFrameBytes)

	first, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false, true)
	require.NoError(t, err)
	require.NotEmpty(t, first.all())
	frame := renderer.liveFrames["draft"]
	require.True(t, frame.reusable())
	require.Positive(t, frame.cost)

	rendered := renderer.renderedBytes

	// A strictly longer body that extends the frame's own content is served from
	// the frame, the version is handed to the background renderer, and the slot is
	// marked stale so a pause in the stream cannot leave the tail missing.
	grown := body + "value700 := 700\n"
	second, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false, true)
	require.NoError(t, err)
	assert.Equal(t, first.all(), second.all(), "the frame on screen stands")
	assert.Equal(t, rendered, renderer.renderedBytes, "the event loop paid for no render")

	miss, ok := renderer.takeLiveMiss()
	require.True(t, ok)
	assert.Equal(t, liveMarkdownContent(grown), miss.content,
		"a live frame renders a large fence without its chroma lexer")
	_, stale := renderer.liveCatchUpDelay()
	assert.True(t, stale, "a served frame schedules a catch-up")

	// The background renderer's result is exact: the next frame pays nothing and
	// the slot stops being stale.
	rows, err := miss.render(newMarkdownRenderer(1))
	require.NoError(t, err)
	require.True(t, renderer.installExactRows(miss, rows, time.Millisecond))

	exact, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false, true)
	require.NoError(t, err)
	assert.Equal(t, wholeRenderRows(t, grown), exact.all(), "the installed frame is exact")
	assert.Equal(t, rendered, renderer.renderedBytes, "an installed version costs no render")
	_, stale = renderer.liveCatchUpDelay()
	assert.False(t, stale, "an exact slot schedules nothing")
}

// TestStaleLiveFrameIsForcedExactByTheCatchUp pins the fallback: when no
// background result is outstanding the catch-up render pays for the tail, so a
// body that stopped growing still reaches its exact frame.
func TestStaleLiveFrameIsForcedExactByTheCatchUp(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := liveFrameProbeBody()
	if _, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false, true); err != nil {
		t.Fatal(err)
	}

	grown := body + "trailing := 1\n"
	if _, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false, true); err != nil {
		t.Fatal(err)
	}
	_, stale := renderer.liveCatchUpDelay()
	require.True(t, stale)

	renderer.forceExact = true
	forced, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false, true)
	require.NoError(t, err)
	assert.Equal(t, wholeRenderRows(t, grown), forced.all(), "the catch-up frame is the current content's")
	_, stale = renderer.liveCatchUpDelay()
	assert.False(t, stale)
	assert.False(t, renderer.forceExact, "the force is consumed by one frame")
}

// wholeRenderRows is the rows a renderer that holds no frame for the body produces,
// which is what a live frame has to equal once it pays for the tail.
func wholeRenderRows(t *testing.T, body string) []string {
	t.Helper()

	rows, err := newMarkdownRenderer(1).renderLiveRows("draft", body, 80, 0, themeDark, false, true)
	require.NoError(t, err)

	return rows.all()
}

// TestLiveFrameRendersWhenItCannotStand pins the invalidation: a frame only stands
// for a strictly longer document that extends it, at the same geometry, theme and
// colour mode. Each case must produce the current content's frame.
func TestLiveFrameRendersWhenItCannotStand(t *testing.T) {
	t.Parallel()

	body := liveFrameProbeBody()
	grown := body + "value700 := 700\n"
	replaced := strings.Repeat("replacement paragraph\n\n", 500)

	cases := map[string]func(*markdownRenderer) (rowSegments, error){
		"a replaced body": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", replaced, 80, 0, themeDark, false, true)
		},
		"a rewound body": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", body[:len(body)/2], 80, 0, themeDark, false, true)
		},
		"another width": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", grown, 100, 0, themeDark, false, true)
		},
		"another inset": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", grown, 80, 2, themeDark, false, true)
		},
		"another theme": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", grown, 80, 0, themeLight, false, true)
		},
		"colour mode": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", grown, 80, 0, themeDark, true, true)
		},
		"another slot": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("other", grown, 80, 0, themeDark, false, true)
		},
	}

	for name, render := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			renderer := newMarkdownRenderer(4)
			_, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false, true)
			require.NoError(t, err)

			rendered := renderer.renderedBytes
			got, err := render(renderer)
			require.NoError(t, err)
			assert.Greater(t, renderer.renderedBytes, rendered, "the frame could not stand")

			fresh, err := render(newMarkdownRenderer(4))
			require.NoError(t, err)
			assert.Equal(t, fresh.all(), got.all(), "the frame is the current content's")
		})
	}
}

// TestLiveFrameReuseIsBounded keeps the reuse map from pinning a long session's
// settled slots.
func TestLiveFrameReuseIsBounded(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := liveFrameProbeBody()

	for slot := range maxLiveFrames + 3 {
		_, err := renderer.renderLiveRows(fmt.Sprintf("slot-%d", slot), body, 80, 0, themeDark, false, true)
		require.NoError(t, err)
	}

	assert.Len(t, renderer.liveFrames, maxLiveFrames)
	_, oldest := renderer.liveFrames["slot-0"]
	assert.False(t, oldest, "the least recently rendered frame is the first to go")
}

// TestReusedLiveFrameIsAWholeRenderOfItsOwnContent pins what the bound actually
// promises. A frame the renderer stands on is the whole render of the content it
// was rendered from — never an old prefix spliced onto a new tail — so the reader
// sees a frame that is one render behind rather than a frame that never existed.
// TestStructuralChangeRendersTheCurrentContent pins the other half: a cheap
// incremental seam produces the current content's frame immediately.
func TestReusedLiveFrameIsAWholeRenderOfItsOwnContent(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := liveFrameProbeBody()
	first, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false, true)
	require.NoError(t, err)

	grown := body + "value700 := 700\n"
	second, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false, true)
	require.NoError(t, err)
	require.Equal(t, first.all(), second.all(), "the frame stands")

	fresh, err := newMarkdownRenderer(4).renderLiveRows("draft", body, 80, 0, themeDark, false, true)
	require.NoError(t, err)
	assert.Equal(t, fresh.all(), second.all(),
		"the frame is the old content's whole render, not a spliced mix")

	whole, err := newMarkdownRenderer(1).render(grown, 80, themeDark, false)
	require.NoError(t, err)
	assert.NotEqual(t, whole, strings.Join(second.all(), "\n"),
		"the frame is one render behind the content it was asked for")
}

// TestStructuralChangeRendersTheCurrentContent pins a small structural change
// after an already-frozen large prefix. Closing and reopening a short fence is
// still cheap, so its current rows must appear without waiting for the worker.
func TestStructuralChangeRendersTheCurrentContent(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := strings.Repeat("An ordinary settled paragraph.\n\n", 600) + "```go\ninitial := 1\n"
	require.Greater(t, len(body), liveFrameBytes)
	if _, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false, true); err != nil {
		t.Fatal(err)
	}
	rendered := renderer.renderedBytes

	grown := body + "```\n\nprose after the fence.\n\n```go\nreopened := 1\n"
	second, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false, true)
	require.NoError(t, err)

	assert.Equal(t, wholeRenderRows(t, grown), second.all(),
		"a cheap structural change is rendered immediately")
	assert.Less(t, renderer.renderedBytes-rendered, liveFrameBytes,
		"the frozen prefix does not participate in the render")
	_, missing := renderer.takeLiveMiss()
	assert.False(t, missing)
}

// A newly freezable boundary can still require rendering a large region. It
// belongs on the bounded worker, and its exact result must paint after a pause.
func TestLargeStructuralChangeDefersUntilExactBackgroundResult(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := liveFrameProbeBody()
	first, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false, true)
	require.NoError(t, err)
	rendered := renderer.renderedBytes
	grown := body + "```\n\nprose after the fence.\n\n```go\nreopened := 1\n"
	second, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false, true)
	require.NoError(t, err)
	assert.Equal(t, rendered, renderer.renderedBytes, "a large newly frozen region cannot block the event loop")
	assert.Equal(t, first.all(), second.all())
	miss, ok := renderer.takeLiveMiss()
	require.True(t, ok)
	assert.Equal(t, liveMarkdownContent(grown), miss.content,
		"a live frame renders a large fence without its chroma lexer")

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
	final, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false, true)
	require.NoError(t, err)
	assert.Equal(t, wholeRenderRows(t, grown), final.all(), "a paused stream reaches the exact whole-render rows")
	assert.Equal(t, rendered, renderer.renderedBytes, "publishing the exact result requires no synchronous render")
	_, stale := renderer.liveCatchUpDelay()
	assert.False(t, stale)
}

// TestSmallLiveBodiesRenderExactlyEveryFrame pins the other side of the bound: a
// body below the reuse threshold is rendered every time, so the streamed output
// stays byte-identical to a whole render.
func TestSmallLiveBodiesRenderExactlyEveryFrame(t *testing.T) {
	t.Parallel()

	document := "Intro paragraph.\n\nSecond paragraph with `code`.\n\nTail.\n"
	streaming := newMarkdownRenderer(4)

	for size := range len(document) + 1 {
		content := document[:size]
		got, err := streaming.renderLive("draft", content, 40, themeDark, false)
		require.NoError(t, err)

		want, err := newMarkdownRenderer(1).render(content, 40, themeDark, false)
		require.NoError(t, err)
		assert.Equal(t, want, got, "size %d", size)
	}

	assert.Less(t, len(document), liveFrameBytes)
	assert.False(t, streaming.liveFrames["draft"].reusable(),
		"a body this small is never served from a previous frame")
	_, stale := streaming.liveCatchUpDelay()
	assert.False(t, stale)
}
