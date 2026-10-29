package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// largeFenceBody builds a labelled fence whose code is exactly bytes long. It is
// the shape a long code answer streams, and the one a live frame renders without
// its chroma lexer.
func largeFenceBody(language string, bytes int) string {
	line := "value := compute(alpha, beta)\n"

	return "```" + language + "\n" + strings.Repeat(line, bytes/len(line)+1)[:bytes]
}

func TestLiveMarkdownContentDropsLargeFenceLanguage(t *testing.T) {
	t.Parallel()

	body := largeFenceBody("go", liveFencePlainBytes)

	live := liveMarkdownContent(body)

	assert.True(t, strings.HasPrefix(live, "```\n"), "the opener keeps its marker and loses the info string")
	assert.NotContains(t, live, "```go")
	assert.Len(t, live, len(body)-len("go"), "only the info string is dropped")
}

func TestLiveMarkdownContentKeepsSmallFenceLanguage(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"small fence":   largeFenceBody("go", liveFencePlainBytes-1),
		"labelled tiny": "Intro.\n\n```go\nfunc main() {}\n```\n\nAfter.\n\nTail.\n",
		"unlabelled":    largeFenceBody("", liveFencePlainBytes*2),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, body, liveMarkdownContent(body))
		})
	}
}

func TestLiveMarkdownContentLeavesProseUntouched(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("ordinary prose with no fence in it. ", 4<<10)

	assert.Equal(t, body, liveMarkdownContent(body))
}

func TestLiveMarkdownContentDropsEveryLargeFence(t *testing.T) {
	t.Parallel()

	large := largeFenceBody("go", liveFencePlainBytes)
	small := "```sh\nls\n```"

	body := strings.Join([]string{"Intro.", large, small, "Middle.", large, "Tail."}, "\n\n")

	live := liveMarkdownContent(body)

	assert.NotContains(t, live, "```go")
	assert.Contains(t, live, "```sh", "a small fence keeps its language")
	assert.Equal(t, 2*len("go"), len(body)-len(live))
}

// TestLiveMarkdownLargeFenceKeepsItsRows pins the contract the transform relies
// on: dropping the info string changes token colours, not the rendered text, the
// indentation or the row count.
func TestLiveMarkdownLargeFenceKeepsItsRows(t *testing.T) {
	t.Parallel()

	body := largeFenceBody("go", liveFencePlainBytes) + "tail line, still streaming"

	labelled, err := newMarkdownRenderer(1).render(body, 60, themeDark, true)
	require.NoError(t, err)

	renderer := newMarkdownRenderer(8)

	rows, err := renderer.renderLiveRows("draft", body, 60, 0, themeDark, true, true)
	require.NoError(t, err)
	require.NotZero(t, rows.count())

	live := strings.Join(rows.all(), "\n")

	assert.Equal(t, ansi.Strip(labelled), ansi.Strip(live), "the visible text is the same")
	assert.Equal(t, strings.Count(labelled, "\n"), strings.Count(live, "\n"), "the row count is the same")
}

// TestLiveMarkdownLargeFenceAvoidsTheLexer proves the transform reaches the
// renderer: a large labelled fence renders through the unlabelled path, so the
// rows it produces are the rows liveMarkdownContent asks for.
func TestLiveMarkdownLargeFenceAvoidsTheLexer(t *testing.T) {
	t.Parallel()

	body := largeFenceBody("go", liveFencePlainBytes)

	renderer := newMarkdownRenderer(8)
	rows, err := renderer.renderLiveRows("draft", body, 60, 0, themeDark, true, true)
	require.NoError(t, err)
	require.NotZero(t, rows.count())

	plain, err := newMarkdownRenderer(1).render(liveMarkdownContent(body), 60, themeDark, true)
	require.NoError(t, err)

	assert.Equal(t, plain, strings.Join(rows.all(), "\n"))
}

// BenchmarkLiveFenceFrame measures one live frame of a growing code fence. The
// live path renders the prepared body (no chroma lexer), which is the shape a
// streaming code answer now costs.
func BenchmarkLiveFenceFrame(b *testing.B) {
	for _, size := range []int{32 << 10, 128 << 10} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			body := largeFenceBody("go", size)
			renderer := newMarkdownRenderer(8)

			// One frame asks for a body the frame cache does not hold, which is what
			// a growing tail costs every time it is rendered.
			frame := func() error {
				delete(renderer.liveFrames, "draft")

				rows, err := renderer.renderLiveRows("draft", body+"// frame\n", 100, 2, themeDark, false, true)
				if err != nil {
					return err
				}

				if rows.count() == 0 {
					return errors.New("no rows")
				}

				return nil
			}

			b.ReportAllocs()

			for b.Loop() {
				if err := frame(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLiveFencePrepare measures the transform itself: one line scan of the
// body per frame, against the render it removes.
func BenchmarkLiveFencePrepare(b *testing.B) {
	body := largeFenceBody("go", 128<<10)

	b.ReportAllocs()

	for b.Loop() {
		if prepared := liveMarkdownContent(body); len(prepared) >= len(body) {
			b.Fatal("the fence was not prepared")
		}
	}
}
