package tui

import (
	"bytes"
	"strings"
	"sync"

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
