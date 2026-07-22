package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSelectionWindowByHeightKeepsWrappedCursorVisible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		heights   []int
		cursor    int
		maximum   int
		wantStart int
		wantEnd   int
	}{
		{name: "top", heights: []int{2, 1, 2, 1}, cursor: 0, maximum: 4, wantStart: 0, wantEnd: 2},
		{name: "middle", heights: []int{2, 1, 2, 1}, cursor: 2, maximum: 4, wantStart: 1, wantEnd: 4},
		{name: "bottom", heights: []int{2, 1, 2, 1}, cursor: 3, maximum: 4, wantStart: 1, wantEnd: 4},
		{name: "single_oversize", heights: []int{8, 1}, cursor: 0, maximum: 3, wantStart: 0, wantEnd: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			start, end := selectionWindowByHeight(test.heights, test.cursor, test.maximum)
			assert.Equal(t, test.wantStart, start)
			assert.Equal(t, test.wantEnd, end)
		})
	}
}

func TestTruncateHeightBoundsRenderedLines(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "one\ntwo", truncateHeight("one\ntwo\nthree", 2))
	assert.Empty(t, truncateHeight("one", 0))
}

func TestTruncateTailHeightKeepsNewestRenderedLines(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "two\nthree", truncateTailHeight("one\ntwo\nthree", 2))
	assert.Equal(t, "one", truncateTailHeight("one", 2))
	assert.Empty(t, truncateTailHeight("one", 0))
}
