package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveThinkingBlock returns the live Thinking block a streaming reasoning delta
// projects into.
func liveThinkingBlock(body string) timelineBlock {
	return timelineBlock{kind: blockThinking, id: draftThinkingID, body: body}
}

// settledThinkingBlock returns the same section once it has stopped growing.
func settledThinkingBlock(body string) timelineBlock {
	return timelineBlock{kind: blockThinking, id: "thinking:1:0", body: body}
}

// liveThinkingCorpus holds the reasoning shapes a streamed thought produces.
func liveThinkingCorpus() map[string]string {
	plain := strings.Repeat("Weighing the options for the next step, sentence after sentence. ", 10)
	return map[string]string{
		"single line":    "One unbroken thought with no line break at all",
		"paragraphs":     "First reasoning paragraph.\n\nSecond reasoning paragraph.\n\nThird one.\n",
		"trailing break": plain + "\n\n",
		"crlf":           "First thought.\r\n\r\nSecond thought.\r\n",
		"blank runs":     "First.\n\n\n\nSecond.\n\nTail.\n",
		"indented":       "First line.\n  an indented continuation line\n\nNext.\n",
		"tabs":           "First\twith a tab inside.\n\nSecond.\n",
		"cjk":            strings.Repeat("正在分析状态更新与界面渲染。\n\n", 8),
		"emoji":          "Analysing 🎯 the target.\n\nChecking 🚀 the launch.\n\nDone.\n",
		"long word":      strings.Repeat("x", 200) + "\n\n" + strings.Repeat("y", 120) + "\n",
		"many lines":     strings.Repeat("A short reasoning line.\n", 40),
		"trailing space": "First line with a trailing space   \n\nSecond.\n",
		"no break yet":   plain,
	}
}

// TestLiveThinkingMatchesSettledSection streams every corpus body one byte at a
// time through the live path and asserts the output is byte-identical to the
// settled section rendered whole at the same content.
func TestLiveThinkingMatchesSettledSection(t *testing.T) {
	t.Parallel()

	for name, body := range liveThinkingCorpus() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, width := range []int{20, 60, 118} {
				for _, noColor := range []bool{false, true} {
					// One renderer across the stream, as the frame loop uses it.
					renderer := newMarkdownRenderer(128)
					for size := 0; size <= len(body); size++ {
						content := body[:size]
						got := renderThinkingBlock(liveThinkingBlock(content), renderer, width, themeDark, noColor)
						want := renderThinkingBlock(settledThinkingBlock(content), nil, width, themeDark, noColor)

						if got != want {
							t.Fatalf("width %d noColor %v size %d:\n got  %q\n want %q",
								width, noColor, size, got, want)
						}
					}
				}
			}
		})
	}
}

// TestLiveThinkingMatchesSettledSectionAcrossFrames covers the frame loop rather
// than a byte-at-a-time stream: several deltas arrive, one frame renders.
func TestLiveThinkingMatchesSettledSectionAcrossFrames(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"First reasoning paragraph with ordinary words.",
		"",
		"Second reasoning paragraph, still growing",
	}, "\n\n")

	renderer := newMarkdownRenderer(128)
	for frame := 1; frame <= len(body); frame++ {
		content := body[:frame]
		got := renderThinkingBlock(liveThinkingBlock(content), renderer, 60, themeDark, false)
		want := renderThinkingBlock(settledThinkingBlock(content), nil, 60, themeDark, false)
		require.Equal(t, want, got, "frame %d content %q", frame, content)
	}
}

// TestLiveThinkingFreezesWholeLines pins the two properties the split rests on,
// so the equivalence test above cannot pass by never freezing.
func TestLiveThinkingFreezesWholeLines(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("A reasoning paragraph that keeps streaming words.\n\n", 40)

	renderer := newMarkdownRenderer(128)
	renderThinkingBlock(liveThinkingBlock(body), renderer, 80, themeDark, false)

	frozen := renderer.thinking
	require.NotEmpty(t, frozen.source, "a body with line breaks must expose a frozen prefix")
	assert.True(t, strings.HasSuffix(frozen.source, "\n"), "a frozen prefix ends on a line break")
	assert.Greater(t, len(frozen.source), len(body)/2, "the frozen prefix must cover most of the body")
	assert.NotEmpty(t, frozen.rows)
	assert.Equal(t, len(frozen.rows), len(frozen.styled))
	assert.Equal(t, len(frozen.rows), len(frozen.widths))
}

// TestLiveThinkingFrameExtendsTheFrozenRows pins the bound: a frame wraps the
// newly streamed region and the live tail, never the whole thought again.
// Wrapping the body per frame would make the total grow with its square.
func TestLiveThinkingFrameExtendsTheFrozenRows(t *testing.T) {
	t.Parallel()

	const frames = 200

	renderer := newMarkdownRenderer(128)

	var body strings.Builder
	wrapped := 0
	for index := range frames {
		fmt.Fprintf(&body, "Reasoning paragraph %d that keeps streaming words.\n\n", index)
		renderer.thinkingWrapped = 0
		renderThinkingBlock(liveThinkingBlock(body.String()), renderer, 80, themeDark, false)
		wrapped += renderer.thinkingWrapped
	}

	assert.LessOrEqual(t, wrapped, 8*body.Len(),
		"the wrapper must not be handed the whole thought on every frame")
}

// TestLiveThinkingDropsFrozenRowsWhenTheBodyIsReplaced pins the invalidation: a
// replaced, rewound or re-geometried body must not reuse the frozen rows.
func TestLiveThinkingDropsFrozenRowsWhenTheBodyIsReplaced(t *testing.T) {
	t.Parallel()

	first := "Alpha thought one.\n\nAlpha thought two.\n\n"
	replaced := "Beta thought one.\n\nBeta thought two.\n\n"
	rewound := "Alpha thought one.\n"

	for name, sequence := range map[string][]string{
		"replaced": {first, replaced, replaced + "Beta thought three.\n\n"},
		"rewound":  {first, rewound, first},
		"geometry": {first, first},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			renderer := newMarkdownRenderer(128)
			for index, content := range sequence {
				width := 60
				if name == "geometry" && index > 0 {
					width = 40
				}

				got := renderThinkingBlock(liveThinkingBlock(content), renderer, width, themeDark, false)
				want := renderThinkingBlock(settledThinkingBlock(content), nil, width, themeDark, false)
				assert.Equal(t, want, got, "content %q width %d", content, width)
			}
		})
	}
}

// TestLiveThinkingSettledSectionsAreNotCached pins that only the live section
// keeps frozen rows: a settled section is rendered once and never extended.
func TestLiveThinkingSettledSectionsAreNotCached(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(128)
	renderThinkingBlock(settledThinkingBlock("Settled thought one.\n\nSettled thought two.\n"), renderer, 60, themeDark, false)

	assert.Empty(t, renderer.thinking.source, "a settled section must not populate the live cache")
}

// FuzzLiveThinkingMatchesSettledSection drives the live section with arbitrary
// reasoning text and compares it with the settled section's whole render.
func FuzzLiveThinkingMatchesSettledSection(f *testing.F) {
	for _, seed := range liveThinkingCorpus() {
		f.Add(seed)
	}
	f.Add("a\n\nb")
	f.Add("x")
	f.Add("\n\n\n")

	f.Fuzz(func(t *testing.T, body string) {
		for _, width := range []int{20, 72} {
			for _, noColor := range []bool{false, true} {
				got := renderThinkingBlock(liveThinkingBlock(body), newMarkdownRenderer(128), width, themeDark, noColor)
				want := renderThinkingBlock(settledThinkingBlock(body), nil, width, themeDark, noColor)

				require.Equal(t, want, got, "width %d noColor %v body %q", width, noColor, body)
			}
		}
	})
}
