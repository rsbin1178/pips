//nolint:wsl_v5 // The bound's cases and their measurements stay adjacent.
package tui

import (
	"fmt"
	"strings"
	"testing"

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

// TestLiveFrameStandsForItsOwnCooldown pins the bound on what one un-freezable
// live tail may cost per frame. A frame whose render is expensive stands for a
// cooldown proportional to that cost, so a streaming block cannot own the event
// loop; the renderer pays for the tail again once the cooldown lapses.
func TestLiveFrameStandsForItsOwnCooldown(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := liveFrameProbeBody()
	require.GreaterOrEqual(t, len(body), liveFrameBytes)

	first, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false)
	require.NoError(t, err)
	require.NotEmpty(t, first.all())
	frame := renderer.liveFrames["draft"]
	require.True(t, frame.reusable())
	require.Positive(t, frame.cost)

	rendered := renderer.renderedBytes

	// A strictly longer body that extends the frame's own content is served from
	// the frame while the cooldown holds.
	grown := body + "value700 := 700\n"
	second, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false)
	require.NoError(t, err)
	assert.Equal(t, first.all(), second.all(), "the frame stands for its cooldown")
	assert.Equal(t, rendered, renderer.renderedBytes, "the second frame paid for no render")

	// Once the cooldown has lapsed the renderer pays for the tail again, and the
	// frame it produces is the current content's.
	renderer.liveFrames["draft"] = liveFrame{
		content: body, rows: first, width: 80, inset: 0,
		theme: themeFingerprint(themeDark.fingerprint), cost: frame.cost,
	}
	expired := renderer.liveFrames["draft"]
	expired.rendered = frame.rendered.Add(-frame.cooldown() - 1)
	renderer.liveFrames["draft"] = expired

	third, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false)
	require.NoError(t, err)
	assert.Greater(t, renderer.renderedBytes, rendered, "the lapsed cooldown pays for the tail again")
	assert.Equal(t, wholeRenderRows(t, grown), third.all(),
		"and the frame it produces is the current content's, exactly")

	// A body that has stopped growing is exact once its own frame's cooldown lapses,
	// and stays exact: this is the half that keeps a turn's last rows from staying one
	// render behind for good.
	final := grown + "trailing := 1\n"
	if _, err := renderer.renderLiveRows("draft", final, 80, 0, themeDark, false); err != nil {
		t.Fatal(err)
	}
	settled := renderer.liveFrames["draft"]
	settled.rendered = settled.rendered.Add(-settled.cooldown() - 1)
	renderer.liveFrames["draft"] = settled

	exact, err := renderer.renderLiveRows("draft", final, 80, 0, themeDark, false)
	require.NoError(t, err)
	assert.Equal(t, wholeRenderRows(t, final), exact.all(), "a settled body is exact")
}

// wholeRenderRows is the rows a renderer that holds no frame for the body produces,
// which is what a live frame has to equal once it pays for the tail.
func wholeRenderRows(t *testing.T, body string) []string {
	t.Helper()

	rows, err := newMarkdownRenderer(1).renderLiveRows("draft", body, 80, 0, themeDark, false)
	require.NoError(t, err)

	return rows.all()
}

// TestLiveFrameRendersWhenItCannotStand pins the invalidation: a frame only
// stands for a strictly longer document that extends it, at the same geometry,
// theme and colour mode. Each case must both pay for a render and produce the
// current content's frame.
func TestLiveFrameRendersWhenItCannotStand(t *testing.T) {
	t.Parallel()

	body := liveFrameProbeBody()
	grown := body + "value700 := 700\n"
	replaced := strings.Repeat("replacement paragraph\n\n", 500)

	cases := map[string]func(*markdownRenderer) (rowSegments, error){
		"a replaced body": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", replaced, 80, 0, themeDark, false)
		},
		"a rewound body": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", body[:len(body)/2], 80, 0, themeDark, false)
		},
		"another width": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", grown, 100, 0, themeDark, false)
		},
		"another inset": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", grown, 80, 2, themeDark, false)
		},
		"another theme": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", grown, 80, 0, themeLight, false)
		},
		"colour mode": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("draft", grown, 80, 0, themeDark, true)
		},
		"another slot": func(r *markdownRenderer) (rowSegments, error) {
			return r.renderLiveRows("other", grown, 80, 0, themeDark, false)
		},
	}

	for name, render := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			renderer := newMarkdownRenderer(4)
			_, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false)
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

// TestLiveFrameNeedsAStrictExtension pins the two conditions a frame's content
// must meet: the new body has to be longer, and it has to start with the frame's
// own body.
func TestLiveFrameNeedsAStrictExtension(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := liveFrameProbeBody()
	_, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false)
	require.NoError(t, err)

	_, unchanged := renderer.reuseLiveFrame("draft", body, 80, 0, themeDark, false)
	assert.False(t, unchanged, "an unchanged body is not served from the frame")

	_, shorter := renderer.reuseLiveFrame("draft", body[:len(body)/2], 80, 0, themeDark, false)
	assert.False(t, shorter, "a shorter body is not served from the frame")

	_, unrelated := renderer.reuseLiveFrame("draft", "different\n"+body, 80, 0, themeDark, false)
	assert.False(t, unrelated, "a body that does not extend the frame's is not served from it")

	_, extended := renderer.reuseLiveFrame("draft", body+"value700 := 700\n", 80, 0, themeDark, false)
	assert.True(t, extended, "a strictly longer body that extends the frame's is served from it")

	_, unnamed := renderer.reuseLiveFrame("", body+"value700 := 700\n", 80, 0, themeDark, false)
	assert.False(t, unnamed, "a frame with no slot is never reused")
}

// TestLiveFrameReuseIsBounded keeps the reuse map from pinning a long session's
// settled slots.
func TestLiveFrameReuseIsBounded(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := liveFrameProbeBody()

	for slot := range maxLiveFrames + 3 {
		_, err := renderer.renderLiveRows(fmt.Sprintf("slot-%d", slot), body, 80, 0, themeDark, false)
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
// The frame is NOT a valid frame for the newer content once the structure changed
// (here a fence that closes, prose, and a fence that reopens), which is why the
// settled frame is the one that has to be exact; TestLiveFrameNeedsAStrictExtension
// and TestSmallLiveBodiesRenderExactlyEveryFrame pin that half.
func TestReusedLiveFrameIsAWholeRenderOfItsOwnContent(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(4)
	body := liveFrameProbeBody()
	first, err := renderer.renderLiveRows("draft", body, 80, 0, themeDark, false)
	require.NoError(t, err)

	grown := body + "```\n\nprose after the fence.\n\n```go\nreopened := 1\n"
	second, err := renderer.renderLiveRows("draft", grown, 80, 0, themeDark, false)
	require.NoError(t, err)
	require.Equal(t, first.all(), second.all(), "the frame stands")

	fresh, err := newMarkdownRenderer(4).renderLiveRows("draft", body, 80, 0, themeDark, false)
	require.NoError(t, err)
	assert.Equal(t, fresh.all(), second.all(),
		"the frame is the old content's whole render, not a spliced mix")

	whole, err := newMarkdownRenderer(1).render(grown, 80, themeDark, false)
	require.NoError(t, err)
	assert.NotEqual(t, whole, strings.Join(second.all(), "\n"),
		"the frame is one render behind the content it was asked for")
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
}
