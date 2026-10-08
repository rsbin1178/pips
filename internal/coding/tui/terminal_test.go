package tui

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

// Recorded output has no live application to consume terminal query replies.
// Consume only queries rather than starting a reader that races emulator Close;
// all rendering, cursor, mode, and color-setting sequences still run normally.
func newScreenReplay(width, height int) *vt.Emulator {
	e := vt.NewEmulator(width, height)
	for _, command := range []int{
		'c', 'n',
		ansi.Command('>', 0, 'c'),
		ansi.Command('?', 0, 'n'),
		ansi.Command(0, '$', 'p'),
		ansi.Command('?', '$', 'p'),
	} {
		e.RegisterCsiHandler(command, func(ansi.Params) bool { return true })
	}

	for _, command := range []int{4, 10, 11, 12} {
		e.RegisterOscHandler(command, func(data []byte) bool {
			return strings.HasSuffix(string(data), ";?")
		})
	}

	return e
}

// reportStrandedAltScreen says which of the two failures this is. A stream that
// never leaves the alternate screen is either truncated before the exit sequences
// or re-entered after them during teardown, and the two need different fixes: the
// first is the harness losing bytes, the second is the application painting after
// it was asked to stop.
func reportStrandedAltScreen(t *testing.T, stream string) {
	t.Helper()

	tail := stream
	if len(tail) > 200 {
		tail = tail[len(tail)-200:]
	}

	t.Logf("alternate screen still current: stream %d bytes, enter=%v, leave=%v, tail=%q",
		len(stream),
		strings.Contains(stream, "\x1b[?1049h"),
		strings.Contains(stream, "\x1b[?1049l"),
		tail)
}
