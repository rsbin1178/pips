package background

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// outputFile is one task's bounded raw-output sink. It is written by the
// executor's dispatcher goroutine and read back by the task's pipeline, so the
// pipeline reproduces the reference's read-a-file-by-offset shape: the file is
// the single source of the events.
type outputFile struct {
	file    *os.File
	path    string
	limit   int64
	written int64
}

func createOutputFile(directory, name string, limit int64) (*outputFile, error) {
	if directory == "" || name == "" || limit <= 0 {
		return nil, fmt.Errorf("%w: incomplete output file", ErrInvalid)
	}

	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("coding background: create output directory: %w", err)
	}

	path := filepath.Join(directory, name)

	//nolint:gosec // The path is a registry-generated name inside the session directory, not caller input.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("coding background: create output file: %w", err)
	}

	return &outputFile{file: file, path: path, limit: limit}, nil
}

// write appends one chunk, stopping at the byte limit. Output past the limit is
// dropped rather than buffered: the bounded projection is the delivery, and the
// process itself is bounded separately by the executor's output ceiling.
func (f *outputFile) write(data []byte) error {
	if f == nil || f.file == nil || len(data) == 0 {
		return nil
	}

	remaining := f.limit - f.written
	if remaining <= 0 {
		return nil
	}

	if int64(len(data)) > remaining {
		data = data[:remaining]
	}

	count, err := f.file.Write(data)
	f.written += int64(count)

	if err != nil {
		return fmt.Errorf("coding background: write output file: %w", err)
	}

	return nil
}

func (f *outputFile) close() error {
	if f == nil || f.file == nil {
		return nil
	}

	err := f.file.Close()
	f.file = nil

	if errors.Is(err, os.ErrClosed) {
		return nil
	}

	return err
}

// readFrom returns up to limit bytes of path past offset, plus the offset to
// resume from. A missing or unreadable file reads as no bytes, so a task whose
// file has not appeared yet keeps ticking.
func readFrom(path string, offset, limit int64) ([]byte, int64, error) {
	if path == "" || limit <= 0 {
		return nil, offset, nil
	}

	//nolint:gosec // The path is a task's own output file inside the session directory.
	file, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || info.Size() <= offset {
		return nil, offset, err
	}

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}

	size := min(info.Size()-offset, limit)
	buffer := make([]byte, size)

	count, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, offset, err
	}

	return buffer[:count], offset + int64(count), nil
}

// tailBytes returns the last limit bytes of path, sanitized for model-visible
// text. It reports nothing when the file cannot be read.
func tailBytes(path string, limit int64) string {
	if path == "" || limit <= 0 {
		return ""
	}

	//nolint:gosec // The path is a task's own output file inside the session directory.
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}

	offset := max(info.Size()-limit, 0)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}

	buffer := make([]byte, info.Size()-offset)

	count, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return ""
	}

	return strings.TrimRight(sanitizeText(string(buffer[:count])), "\n")
}

// sanitizeText replaces an invalid rune and any control character other than
// tab, newline, and carriage return, mirroring the rune-level sanitization the
// shell result already applies to captured output.
func sanitizeText(value string) string {
	if value == "" {
		return ""
	}

	var output strings.Builder
	output.Grow(len(value))

	for len(value) > 0 {
		r, width := utf8.DecodeRuneInString(value)
		if r == utf8.RuneError && width == 1 {
			output.WriteByte('?')

			value = value[1:]

			continue
		}

		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0x7f {
			output.WriteByte('?')
		} else {
			output.WriteRune(r)
		}

		value = value[width:]
	}

	return output.String()
}
