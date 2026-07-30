package sshclient

import "bytes"

var (
	bracketedPasteStart = []byte("\x1b[200~")
	bracketedPasteEnd   = []byte("\x1b[201~")
)

// InputFilter forwards terminal bytes while converting Ctrl+V outside a
// bracketed paste into local upload requests.
type InputFilter struct {
	inPaste bool
	pending []byte
}

// Push consumes one arbitrary input chunk and returns forwarded bytes plus the
// number of explicit local image-paste requests it contained.
func (f *InputFilter) Push(input []byte) ([]byte, int) {
	output := make([]byte, 0, len(input))
	requests := 0

	for _, value := range input {
		f.pending = append(f.pending, value)
		f.drain(&output, &requests, false)
	}

	return output, requests
}

// Flush releases any partial escape sequence at end of input.
func (f *InputFilter) Flush() ([]byte, int) {
	output := make([]byte, 0, len(f.pending))
	requests := 0
	f.drain(&output, &requests, true)

	return output, requests
}

func (f *InputFilter) drain(output *[]byte, requests *int, flush bool) {
	for len(f.pending) > 0 {
		marker := bracketedPasteStart
		if f.inPaste {
			marker = bracketedPasteEnd
		}

		if bytes.Equal(f.pending, marker) {
			*output = append(*output, f.pending...)
			f.pending = f.pending[:0]
			f.inPaste = !f.inPaste

			return
		}

		if !flush && bytes.HasPrefix(marker, f.pending) {
			return
		}

		value := f.pending[0]
		f.pending = f.pending[1:]

		if !f.inPaste && value == 0x16 {
			*requests++
		} else {
			*output = append(*output, value)
		}
	}
}
