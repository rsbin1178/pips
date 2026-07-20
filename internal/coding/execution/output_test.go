package execution

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStreamCaptureRetainsOwnedHeadAndTail(t *testing.T) {
	t.Parallel()

	capture := newStreamCapture(10)
	input := []byte("abcdefghijklmnopqrstuvwxyz")
	assert.EqualValues(t, 0, capture.write(input[:8]))
	assert.EqualValues(t, 8, capture.write(input[8:]))
	input[0] = 'X'

	result := capture.result()
	assert.Equal(t, []byte("abcde"), result.Head())
	assert.Equal(t, []byte("vwxyz"), result.Tail())
	assert.EqualValues(t, 26, result.TotalBytes())
	assert.True(t, result.Truncated())

	head := result.Head()
	head[0] = 'Y'

	assert.Equal(t, []byte("abcde"), result.Head())
}

func TestOutputCollectorTracksStreamsAndHardLimit(t *testing.T) {
	t.Parallel()

	collector := newOutputCollector(OutputLimits{CaptureBytes: 4, MaxBytes: 5})
	offset, exceeded := collector.write(StreamStdout, []byte("abc"))
	assert.EqualValues(t, 0, offset)
	assert.False(t, exceeded)
	offset, exceeded = collector.write(StreamStderr, []byte("def"))
	assert.EqualValues(t, 0, offset)
	assert.True(t, exceeded)

	stdout, stderr := collector.results()
	assert.EqualValues(t, 3, stdout.TotalBytes())
	assert.EqualValues(t, 3, stderr.TotalBytes())
}
