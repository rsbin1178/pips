package sse_test

import (
	"bufio"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/rsbin/pips/ai/internal/sse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func collect(t *testing.T, input string, opts ...sse.Option) []sse.Event {
	t.Helper()

	var events []sse.Event

	for ev, err := range sse.NewParser(strings.NewReader(input), opts...).All() {
		require.NoError(t, err)

		events = append(events, ev)
	}

	return events
}

func TestParserBasics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  []sse.Event
	}{
		{
			name:  "single event",
			input: "data: hello\n\n",
			want:  []sse.Event{{Type: "message", Data: "hello"}},
		},
		{
			name:  "named event with id",
			input: "event: content_block_delta\nid: 42\ndata: {\"x\":1}\n\n",
			want:  []sse.Event{{Type: "content_block_delta", Data: `{"x":1}`, ID: "42"}},
		},
		{
			name:  "multiple data lines join with newline",
			input: "data: line1\ndata: line2\n\n",
			want:  []sse.Event{{Type: "message", Data: "line1\nline2"}},
		},
		{
			name:  "crlf line endings",
			input: "event: ping\r\ndata: {}\r\n\r\n",
			want:  []sse.Event{{Type: "ping", Data: "{}"}},
		},
		{
			name:  "comment lines ignored",
			input: ": keepalive\ndata: real\n\n: another comment\n\n",
			want:  []sse.Event{{Type: "message", Data: "real"}},
		},
		{
			name:  "no space after colon",
			input: "data:tight\n\n",
			want:  []sse.Event{{Type: "message", Data: "tight"}},
		},
		{
			name:  "only first space stripped",
			input: "data:  two spaces\n\n",
			want:  []sse.Event{{Type: "message", Data: " two spaces"}},
		},
		{
			name:  "empty data field dispatches",
			input: "data:\n\n",
			want:  []sse.Event{{Type: "message", Data: ""}},
		},
		{
			name:  "multiple events",
			input: "data: a\n\ndata: b\n\n",
			want:  []sse.Event{{Type: "message", Data: "a"}, {Type: "message", Data: "b"}},
		},
		{
			name:  "final event without trailing blank line is emitted",
			input: "data: last",
			want:  []sse.Event{{Type: "message", Data: "last"}},
		},
		{
			name:  "openai done sentinel",
			input: "data: {\"choices\":[]}\n\ndata: [DONE]\n\n",
			want:  []sse.Event{{Type: "message", Data: `{"choices":[]}`}, {Type: "message", Data: "[DONE]"}},
		},
		{
			name:  "unknown field ignored",
			input: "data: x\nfancy: y\n\n",
			want:  []sse.Event{{Type: "message", Data: "x"}},
		},
		{
			name:  "retry field captured",
			input: "retry: 3000\ndata: x\n\n",
			want:  []sse.Event{{Type: "message", Data: "x", Retry: "3000"}},
		},
		{
			name:  "blank lines between events do not emit empties",
			input: "\n\n\ndata: x\n\n\n\n",
			want:  []sse.Event{{Type: "message", Data: "x"}},
		},
		{
			name:  "empty input",
			input: "",
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, collect(t, tc.input))
		})
	}
}

func TestParserLargeLine(t *testing.T) {
	t.Parallel()

	// A data line well beyond bufio's 64 KiB default must parse fine with the
	// default 1 MiB ceiling.
	big := strings.Repeat("x", 200_000)
	events := collect(t, "data: "+big+"\n\n")
	require.Len(t, events, 1)
	assert.Len(t, events[0].Data, 200_000)
}

func TestParserLineTooLong(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 10_000)
	parser := sse.NewParser(strings.NewReader("data: "+big+"\n\n"), sse.WithMaxLineSize(1024))

	var lastErr error

	for _, err := range parser.All() {
		if err != nil {
			lastErr = err
			break
		}
	}

	require.Error(t, lastErr)
	assert.ErrorIs(t, lastErr, bufio.ErrTooLong)
}

func TestParserEarlyBreak(t *testing.T) {
	t.Parallel()

	input := "data: one\n\ndata: two\n\ndata: three\n\n"

	var got []string

	for ev, err := range sse.NewParser(strings.NewReader(input)).All() {
		require.NoError(t, err)

		got = append(got, ev.Data)
		if len(got) == 2 {
			break
		}
	}

	assert.Equal(t, []string{"one", "two"}, got)
}

type failingReader struct {
	data string
	err  error
	done bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}

	return 0, r.err
}

func TestParserReadError(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset")
	parser := sse.NewParser(&failingReader{data: "data: partial\n\n", err: boom})

	var (
		events  []sse.Event
		lastErr error
	)

	for ev, err := range parser.All() {
		if err != nil {
			lastErr = err
			break
		}

		events = append(events, ev)
	}

	require.Len(t, events, 1)
	assert.Equal(t, "partial", events[0].Data)
	assert.ErrorIs(t, lastErr, boom)
}

func FuzzParse(f *testing.F) {
	f.Add("data: hello\n\n")
	f.Add("event: e\r\ndata: {\"a\":1}\r\n\r\n")
	f.Add(": comment\ndata:\nb\n\nid: 7\ndata: x\n\n")
	f.Add("data: [DONE]\n\n")
	f.Add("\r\r\n\r")
	f.Add("event\ndata\n\n")

	f.Fuzz(func(t *testing.T, input string) {
		parser := sse.NewParser(strings.NewReader(input), sse.WithMaxLineSize(1<<16))
		for ev, err := range parser.All() {
			if err != nil {
				break
			}
			// Invariants: a dispatched event always has a type, and its data
			// never contains a bare CR (line splitting removed terminators).
			if ev.Type == "" {
				t.Fatalf("dispatched event with empty type: %+v", ev)
			}

			if strings.ContainsRune(ev.Data, '\r') {
				t.Fatalf("event data contains CR: %q", ev.Data)
			}
		}
	})
}

// Guard that Parser.All satisfies the iter.Seq2 shape used by ai.Stream.
var _ iter.Seq2[sse.Event, error] = (*sse.Parser)(nil).All()
