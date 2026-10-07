package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveMarkdownCorpus holds the document shapes a streaming answer or reasoning
// section produces, plus the constructs whose rendering could depend on what
// follows a boundary.
func liveMarkdownCorpus() map[string]string {
	return map[string]string{
		"paragraphs": "First paragraph of the answer.\n\nSecond paragraph of the answer.\n\nThird one.\n",
		"single":     "Only one paragraph, still streaming",
		"atx":        "# Heading\n\nBody paragraph under the heading.\n\nAnother paragraph.\n",
		"setext":     "Title\n=====\n\nBody.\n\nSub\n---\n\nMore.\n",
		"tight list": "Intro paragraph.\n\n- item one\n- item two\n\nAfter the list.\n",
		"loose list": "Intro.\n\n- item one\n\n- item two\n\nAfter.\n\nClosing paragraph.\n",
		"nested":     "Intro.\n\n- outer\n  - inner\n\nAfter the nesting.\n\nTail.\n",
		"ordered":    "Steps:\n\n1. first\n2. second\n\nDone.\n\nMore prose.\n",
		"fence":      "Intro.\n\n```go\nfunc main() {}\n```\n\nAfter the code.\n\nTail.\n",
		"fence info": "Intro.\n\n```go title=x\nx := 1\n```\n\nAfter.\n\nTail.\n",
		"tilde":      "Intro.\n\n~~~\nplain\n~~~\n\nAfter.\n\nTail.\n",
		"indented":   "Intro.\n\n    indented code\n\nAfter.\n\nTail.\n",
		"quote":      "Intro.\n\n> quoted line\n\nAfter the quote.\n\nTail.\n",
		"table":      "Intro.\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\nAfter the table.\n\nTail.\n",
		"rule":       "Intro.\n\n---\n\nAfter the rule.\n\nTail.\n",
		"inline":     "A **bold** start.\n\nA [link](https://example.com) and `code`.\n\nTail.\n",
		"definition": "[target]: https://example.com\n\nSee [target] here.\n\nTail.\n",
		"def later":  "Intro.\n\n[target]: https://example.com\n\nSee [target].\n\nTail.\n",
		// A usage can precede its definition, and a definition's label can span
		// lines: goldmark resolves references over the whole document, so neither
		// may end up on the opposite side of a frozen boundary from the other.
		"forward ref":     "Intro.\n\nUse [later] first.\n\n[later]: /url\n\nTail.\n",
		"multi label":     "A.\n\n[x\ny]: https://e.com \"t\"\n\nB.\n\nC uses [x y] here.\n",
		"code bracket":    "Intro.\n\n```go\narr[0] = 1\n```\n\nAfter.\n\nTail.\n",
		"shortcut refs":   "Intro paragraph.\n\nSee [one] and [two] and [three].\n\n[one]: /a\n\n[two]: /b\n\nTail.\n",
		"html":            "Intro.\n\n<div>block</div>\n\nAfter.\n\nTail.\n",
		"cjk":             "第一段说明文字，包含中文与标点。\n\n第二段继续说明，保持宽度测量。\n\n第三段结束。\n",
		"emoji":           "Intro with 🎯 emoji.\n\nSecond 🚀 paragraph.\n\nTail.\n",
		"hard break":      "Line one  \nline two\n\nSecond paragraph.\n\nTail.\n",
		"heading tail":    "Intro.\n\n## Section\n\nBody.\n\n### Sub\n\nMore body.\n\nTail.\n",
		"blank lines":     "Intro.\n\n\n\nAfter extra blanks.\n\nTail.\n",
		"form feed":       "Intro.\n\n\f\nAfter a form feed line.\n\nTail.\n",
		"vertical tab":    "Intro.\n\n\v\nAfter a vertical tab.\n\nTail.\n",
		"crlf":            "Intro.\r\n\r\nSecond paragraph.\r\n\r\nTail.\r\n",
		"list then prose": "Intro.\n\n- a\n- b\n\nProse paragraph after the list.\n\nTail paragraph.\n",
	}
}

// TestLiveMarkdownMatchesWholeRender streams every corpus document one byte at a
// time through the live renderer and asserts the output is byte-identical to a
// whole-document render at the same content.
func TestLiveMarkdownMatchesWholeRender(t *testing.T) {
	t.Parallel()

	for name, document := range liveMarkdownCorpus() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, width := range []int{24, 40, 118} {
				// One renderer across the whole stream, so the frozen prefix and
				// its cache are exercised the way a frame loop uses them.
				streaming := newMarkdownRenderer(128)
				for size := 0; size <= len(document); size++ {
					content := document[:size]
					got, err := streaming.renderLive("draft", content, width, themeDark, false)
					require.NoError(t, err)

					want, err := newMarkdownRenderer(1).render(content, width, themeDark, false)
					require.NoError(t, err)

					if got != want {
						t.Fatalf("width %d size %d:\n got  %q\n want %q", width, size, got, want)
					}
				}
			}
		})
	}
}

// TestLiveMarkdownMatchesWholeRenderWithColor repeats the streaming equivalence
// with styling on, because colour changes the padding glamour emits.
func TestLiveMarkdownMatchesWholeRenderWithColor(t *testing.T) {
	t.Parallel()

	document := strings.Join([]string{
		"First paragraph with **bold** text.",
		"Second paragraph with `code` and a [link](https://example.com).",
		"第三段包含中文。",
		"Fourth paragraph, still growing",
	}, "\n\n")

	streaming := newMarkdownRenderer(128)
	for size := 0; size <= len(document); size++ {
		content := document[:size]
		got, err := streaming.renderLive("draft", content, 60, themeDark, true)
		require.NoError(t, err)

		want, err := newMarkdownRenderer(1).render(content, 60, themeDark, true)
		require.NoError(t, err)

		require.Equal(t, want, got, "size %d", size)
	}
}

// TestLiveMarkdownFreezesLongPrefixes asserts the boundary scanner actually
// finds a freeze point, so the equivalence test above is not passing by always
// falling back to a whole render.
func TestLiveMarkdownFreezesLongPrefixes(t *testing.T) {
	t.Parallel()

	paragraphs := make([]string, 0, 40)
	for index := range 40 {
		paragraphs = append(paragraphs, fmt.Sprintf("Paragraph %d of a long answer that keeps streaming.", index))
	}

	document := strings.Join(paragraphs, "\n\n") + "\n\nstill growing"

	prefixEnd, block, ok := liveMarkdownBoundary(document)
	require.True(t, ok, "a paragraph-only body must expose a freeze boundary")
	assert.Greater(t, prefixEnd, len(document)/2, "the boundary must freeze most of the body")
	assert.Equal(t, "Paragraph 39 of a long answer that keeps streaming.", block)

	renderer := newMarkdownRenderer(128)
	_, err := renderer.renderLive("draft", document, 80, themeDark, false)
	require.NoError(t, err)

	frozen, exists := renderer.frozen["draft"]
	require.True(t, exists, "the live slot must hold its frozen prefix")
	assert.Equal(t, document[:prefixEnd], frozen.source)
	assert.NotEmpty(t, frozen.rows)
	assert.Equal(t, block, frozen.block)
}

// TestLiveMarkdownFrameExtendsTheFrozenPrefix pins the bound: a frame renders the
// newly frozen region and the live tail, never the whole body again. Rendering
// the body per frame would make the total grow with the square of its length.
func TestLiveMarkdownFrameExtendsTheFrozenPrefix(t *testing.T) {
	t.Parallel()

	const frames = 200

	renderer := newMarkdownRenderer(128)
	renderer.renderedBytes = 0

	var body strings.Builder
	for index := range frames {
		fmt.Fprintf(&body, "Paragraph %d of a long answer that keeps streaming.\n\n", index)
		_, err := renderer.renderLive("draft", body.String(), 80, themeDark, false)
		require.NoError(t, err)
	}

	// Each frame freezes one more paragraph, so a whole-prefix render would total
	// roughly frames^2/2 paragraph lengths; extending renders a constant amount.
	assert.LessOrEqual(t, renderer.renderedBytes, 20*body.Len(),
		"the engine must not be handed the whole body on every frame")
}

// TestLiveMarkdownBoundaryRejectsUnsafeBlocks pins the freeze rule: a block
// whose rendering can still change when more text arrives must never end a
// frozen prefix. The scanner may still freeze an earlier safe block, which is
// what keeps the render exact.
func TestLiveMarkdownBoundaryRejectsUnsafeBlocks(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct{ document, unsafe string }{
		"heading":          {"Intro.\n\n## Section\n\nmore", "## Section"},
		"fenced code":      {"Intro.\n\n```\ncode\n```\n\nmore", "```\ncode\n```"},
		"indented code":    {"Intro.\n\n    code\n\nmore", "    code"},
		"open fence":       {"Intro.\n\n```\ncode\n\nmore", "```\ncode"},
		"bullet list":      {"Intro.\n\n- item\n\nmore", "- item"},
		"ordered list":     {"Intro.\n\n1. item\n\nmore", "1. item"},
		"block quote":      {"Intro.\n\n> quote\n\nmore", "> quote"},
		"table":            {"Intro.\n\n| a | b |\n| - | - |\n\nmore", "| a | b |"},
		"thematic break":   {"Intro.\n\n***\n\nmore", "***"},
		"setext underline": {"Intro.\n\nBody\n====\n\nmore", "Body\n===="},
		"html block":       {"Intro.\n\n<div>\n\nmore", "<div>"},
		"definition":       {"Intro.\n\n[t]: https://x\n\nSee [t].\n\nmore", "[t]: https://x"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			prefixEnd, block, ok := liveMarkdownBoundary(testCase.document)
			if ok {
				assert.True(t, freezableMarkdownBlock(block), "a frozen block must be freezable")
				assert.NotEqual(t, testCase.unsafe, block, "an unsafe block must never end a frozen prefix")
				assert.Less(t, prefixEnd, len(testCase.document))
			}

			// Whatever the scanner decides, the render must stay exact.
			renderer := newMarkdownRenderer(128)
			got, err := renderer.renderLive("draft", testCase.document, 40, themeDark, false)
			require.NoError(t, err)
			want, err := newMarkdownRenderer(1).render(testCase.document, 40, themeDark, false)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

// TestLiveMarkdownFallsBackWhenNoBoundaryExists covers the single-paragraph
// case, where no boundary can be frozen.
func TestLiveMarkdownFallsBackWhenNoBoundaryExists(t *testing.T) {
	t.Parallel()

	_, _, ok := liveMarkdownBoundary("one long paragraph with no blank line at all")
	assert.False(t, ok)
}

// liveMarkdownReference renders content whole, the way a settled record does.
func liveMarkdownReference(t *testing.T, content string, width int, noColor bool) string {
	t.Helper()

	rendered, err := newMarkdownRenderer(1).render(content, width, themeDark, noColor)
	require.NoError(t, err)

	return rendered
}

// FuzzLiveMarkdownMatchesWholeRender drives the live renderer with arbitrary
// content and compares it with a whole-document render.
//
// Two comparisons are made. The colour-free render must match byte for byte.
// The coloured render is compared with its escape sequences removed, because
// the pinned fork resolves a fence's unrecognised language through chroma's
// lexer registry, whose analyser tie-break follows Go's map order; that picks a
// different colour index between two calls and is glamour's behaviour, not this
// renderer's (it reproduces on an unmodified checkout).
func FuzzLiveMarkdownMatchesWholeRender(f *testing.F) {
	for _, seed := range liveMarkdownCorpus() {
		f.Add(seed)
	}
	f.Add("```\nunclosed\n\n# heading\n\ntext")
	f.Add("a\n\n> q\n\n- l\n\n1. o\n\n| t |\n\n---\n\ntext")

	f.Fuzz(func(t *testing.T, content string) {
		for _, width := range []int{20, 72} {
			got, err := newMarkdownRenderer(128).renderLive("draft", content, width, themeDark, true)
			require.NoError(t, err)
			require.Equal(t, liveMarkdownReference(t, content, width, true), got,
				"no-colour width %d content %q", width, content)

			colored, err := newMarkdownRenderer(128).renderLive("draft", content, width, themeDark, false)
			require.NoError(t, err)
			require.Equal(t,
				ansi.Strip(liveMarkdownReference(t, content, width, false)),
				ansi.Strip(colored),
				"colour width %d content %q", width, content)
		}
	})
}

// TestLiveMarkdownFrozenPrefixResetsWhenTheBodyIsReplaced pins the invalidation:
// a slot's frozen rows describe one body, and a body that is replaced, rewound
// or re-rendered at another geometry must not reuse them.
func TestLiveMarkdownFrozenPrefixResetsWhenTheBodyIsReplaced(t *testing.T) {
	t.Parallel()

	first := "Alpha paragraph one.\n\nAlpha paragraph two.\n\nAlpha paragraph three.\n\n"
	replaced := "Beta paragraph one.\n\nBeta paragraph two.\n\nBeta paragraph three.\n\n"
	rewound := "Alpha paragraph one.\n\nAlpha paragraph two.\n"

	for name, sequence := range map[string][]string{
		"replaced":     {first, replaced, replaced + "Beta paragraph four.\n\n"},
		"rewound":      {first, rewound, first},
		"new message":  {first, "A fresh answer with no relation to the previous one.\n\nSecond line of it.\n\n"},
		"same content": {first, first, first + "More text after the first body.\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			renderer := newMarkdownRenderer(128)
			for _, content := range sequence {
				got, err := renderer.renderLive("draft", content, 60, themeDark, false)
				require.NoError(t, err)
				assert.Equal(t, liveMarkdownReference(t, content, 60, false), got,
					"content %q", content)
			}
		})
	}
}

// TestLiveMarkdownFrozenPrefixFollowsGeometryChanges pins that a width, theme or
// colour-mode change discards the frozen rows rather than extending them.
func TestLiveMarkdownFrozenPrefixFollowsGeometryChanges(t *testing.T) {
	t.Parallel()

	renderer := newMarkdownRenderer(128)
	body := "First paragraph of the answer.\n\nSecond paragraph of the answer.\n\n"

	for _, step := range []struct {
		width   int
		theme   colorTheme
		noColor bool
	}{
		{60, themeDark, false},
		{60, themeDark, true},
		{40, themeDark, false},
		{40, themeLight, false},
		{60, themeDark, false},
	} {
		got, err := renderer.renderLive("draft", body, step.width, step.theme, step.noColor)
		require.NoError(t, err)

		want, err := newMarkdownRenderer(1).render(body, step.width, step.theme, step.noColor)
		require.NoError(t, err)
		assert.Equal(t, want, got, "width %d noColor %v", step.width, step.noColor)
	}
}

// TestLiveMarkdownBoundaryStopsAtALinkReference pins the freeze rule for link
// references: goldmark resolves them over the whole document, so a bracket on
// either side of a boundary can change the other side's rows. A usage before its
// definition, a definition whose label spans lines, and a definition that a later
// usage relies on all have to stop the freeze.
func TestLiveMarkdownBoundaryStopsAtALinkReference(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct{ document, wantBlock string }{
		"forward reference":  {"Intro.\n\nUse [later] first.\n\n[later]: /url\n\nTail.\n", "Intro."},
		"multiline label":    {"A.\n\n[x\ny]: https://e.com \"t\"\n\nB.\n\nC uses [x y] here.\n", "A."},
		"definition first":   {"Intro.\n\n[t]: https://x\n\nSee [t].\n\nmore", "Intro."},
		"usage after first":  {"Intro paragraph.\n\nSee [one] here.\n\n[one]: /a\n\nTail.\n", "Intro paragraph."},
		"no definition":      {"Intro.\n\nA [label] with no definition anywhere.\n\nTail.\n", "Intro."},
		"bracket in a fence": {"Intro.\n\n```go\narr[0] = 1\n```\n\nAfter.\n", "Intro."},
		"plain prose":        {"First paragraph.\n\nSecond paragraph.\n\nstill growing", "Second paragraph."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			prefixEnd, block, ok := liveMarkdownBoundary(testCase.document)
			require.True(t, ok, "the document must expose a freeze boundary")
			assert.Equal(t, testCase.wantBlock, block)
			assert.LessOrEqual(t, prefixEnd, len(testCase.document))

			// Whatever the scanner decides, the assembled render must equal the
			// whole render, which is the property the freeze rule exists for.
			renderer := newMarkdownRenderer(128)
			got, err := renderer.renderLive("draft", testCase.document, 60, themeDark, false)
			require.NoError(t, err)
			assert.Equal(t, liveMarkdownReference(t, testCase.document, 60, false), got)
		})
	}
}

// TestLiveMarkdownRowsMatchTheStringRender pins the managed store's row path: the
// rows it is handed must be exactly the rows of the string render, so consuming
// rows instead of a joined document cannot change what is drawn.
func TestLiveMarkdownRowsMatchTheStringRender(t *testing.T) {
	t.Parallel()

	for name, document := range liveMarkdownCorpus() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, width := range []int{24, 40, 118} {
				streaming := newMarkdownRenderer(128)
				rowRenderer := newMarkdownRenderer(128)
				for size := 0; size <= len(document); size++ {
					content := document[:size]
					want, err := streaming.renderLive("draft", content, width, themeDark, false)
					require.NoError(t, err)

					rows, err := rowRenderer.renderLiveRows("draft", content, width, 0, themeDark, false)
					require.NoError(t, err)

					require.Equal(t, splitTranscriptRows(want), rows.all(), "width %d size %d", width, size)
				}
			}
		})
	}
}

// TestLiveEntryRowsMatchTheWholeRender streams a draft block through the store's
// row path and asserts it equals a whole-document render of the entry, inset
// included, at every size.
func TestLiveEntryRowsMatchTheWholeRender(t *testing.T) {
	t.Parallel()

	document := strings.Join([]string{
		"First paragraph of the streaming answer.",
		"Second paragraph with a **bold** run.",
		"Third paragraph, still streaming",
	}, "\n\n")

	const width = 118
	streaming := newMarkdownRenderer(128)
	for size := 0; size <= len(document); size++ {
		block := timelineBlock{kind: blockDraft, id: "draft", body: document[:size]}

		rows, ok := liveEntryRows(block, streaming, width, themeDark, false)
		require.True(t, ok)

		want := splitTranscriptRows(renderTimelineEntry(
			block, newMarkdownRenderer(1), width, themeDark, false, timelineRenderOptions{},
		))
		require.Equal(t, want, rows.all(), "size %d", size)
	}
}

// TestRowSegmentsWindow pins the two-segment row view the live record hands the
// store: a window inside one segment is a subslice, a window across the seam is
// the two in order, and out-of-range bounds clamp instead of panicking.
func TestRowSegmentsWindow(t *testing.T) {
	t.Parallel()

	segments := rowSegments{frozen: []string{"a", "b", "c"}, tail: []string{"d", "e"}}

	assert.Equal(t, 5, segments.count())
	assert.Equal(t, []string{"b", "c"}, segments.window(1, 3))
	assert.Equal(t, []string{"c", "d"}, segments.window(2, 4))
	assert.Equal(t, []string{"d", "e"}, segments.window(3, 5))
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, segments.window(0, 99))
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, segments.all())
	assert.Nil(t, segments.window(4, 4))
	assert.Nil(t, segments.window(9, 12))

	flat := rowSegments{frozen: []string{"x", "y"}}
	assert.Equal(t, []string{"x"}, flat.window(0, 1))
	assert.Equal(t, []string{"x", "y"}, flat.all())
}

// TestLiveThinkingEntryRowsMatchTheWholeRender streams a live thought through the
// store's row path and asserts it equals a whole-section render of the entry,
// glyph, padding and inset included, at every size and in both colour modes.
func TestLiveThinkingEntryRowsMatchTheWholeRender(t *testing.T) {
	t.Parallel()

	document := strings.Join([]string{
		"第一个思考段落，包含中文与标点。",
		"Second thought paragraph with ordinary words.",
		"Third thought paragraph, still streaming",
	}, "\n\n")

	for _, noColor := range []bool{false, true} {
		for _, width := range []int{24, 60, 118} {
			streaming := newMarkdownRenderer(128)
			for size := 0; size <= len(document); size++ {
				block := timelineBlock{kind: blockThinking, id: draftThinkingID, body: document[:size]}

				rows, ok := liveEntryRows(block, streaming, width, themeDark, noColor)
				require.True(t, ok)

				want := splitTranscriptRows(renderTimelineEntry(
					block, newMarkdownRenderer(1), width, themeDark, noColor, timelineRenderOptions{},
				))
				require.Equal(t, want, rows.all(),
					"noColor %v width %d size %d", noColor, width, size)
			}
		}
	}
}
