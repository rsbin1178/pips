// Package patch parses and applies the bounded, model-facing patch grammar
// used by the coding application. It performs no filesystem I/O.
package patch

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/workspace"
)

var (
	// ErrInvalid means a patch does not conform to the supported grammar.
	ErrInvalid = errors.New("coding patch: invalid patch")
	// ErrConflict means an update hunk does not identify one exact location.
	ErrConflict = errors.New("coding patch: context conflict")
	// ErrBinary means a patch or target contains non-text content.
	ErrBinary = errors.New("coding patch: binary content")
)

// Kind identifies one supported file operation.
type Kind uint8

// Supported file operation kinds.
const (
	Add Kind = iota + 1
	Update
	Delete
)

// Line is one exact hunk line. Operation is space, plus, or minus.
type Line struct {
	Operation byte
	Text      string
}

// Hunk is one ordered exact-context update.
type Hunk struct {
	Lines []Line
}

// Change is one file operation in patch order.
type Change struct {
	Kind    Kind
	Path    string
	Content []byte
	Hunks   []Hunk
}

// Document is one fully parsed patch.
type Document struct {
	Changes []Change
}

// Limits bounds parser input and file operations.
type Limits struct {
	Bytes int
	Files int
}

// Parse validates the complete patch before returning any operation.
//
//nolint:gocyclo // The strict parser is an explicit bounded state machine over three operation grammars.
func Parse(value string, limits Limits) (Document, error) {
	if limits.Bytes <= 0 || limits.Files <= 0 {
		return Document{}, fmt.Errorf("%w: limits must be positive", ErrInvalid)
	}

	if len(value) > limits.Bytes {
		return Document{}, fmt.Errorf("%w: input exceeds %d bytes", ErrInvalid, limits.Bytes)
	}

	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return Document{}, ErrBinary
	}

	normalized := strings.ReplaceAll(value, "\r\n", "\n")
	if strings.ContainsRune(normalized, '\r') {
		return Document{}, fmt.Errorf("%w: lone carriage return", ErrInvalid)
	}

	lines := strings.Split(normalized, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) < 2 || lines[0] != "*** Begin Patch" || lines[len(lines)-1] != "*** End Patch" {
		return Document{}, fmt.Errorf("%w: missing Begin/End markers", ErrInvalid)
	}

	document := Document{}
	seen := make(map[string]struct{})

	for index := 1; index < len(lines)-1; {
		line := lines[index]

		kind, rawPath, ok := parseHeader(line)
		if !ok {
			return Document{}, fmt.Errorf("%w: unexpected line %d", ErrInvalid, index+1)
		}

		name, err := workspace.NormalizePath(strings.TrimSpace(rawPath), false)
		if err != nil {
			return Document{}, fmt.Errorf("%w: line %d: %w", ErrInvalid, index+1, err)
		}

		if _, duplicate := seen[name]; duplicate {
			return Document{}, fmt.Errorf("%w: duplicate target %q", ErrInvalid, name)
		}

		seen[name] = struct{}{}

		if len(document.Changes) >= limits.Files {
			return Document{}, fmt.Errorf("%w: more than %d file operations", ErrInvalid, limits.Files)
		}

		index++

		change := Change{Kind: kind, Path: name}
		switch kind {
		case Add:
			var content []string

			for index < len(lines)-1 && !isFileHeader(lines[index]) {
				if !strings.HasPrefix(lines[index], "+") {
					return Document{}, fmt.Errorf("%w: add line %d must start with +", ErrInvalid, index+1)
				}

				content = append(content, lines[index][1:])
				index++
			}

			if len(content) > 0 {
				change.Content = []byte(strings.Join(content, "\n") + "\n")
			}

		case Delete:
			if index < len(lines)-1 && !isFileHeader(lines[index]) {
				return Document{}, fmt.Errorf("%w: delete %q has unexpected body", ErrInvalid, name)
			}

		case Update:
			for index < len(lines)-1 && !isFileHeader(lines[index]) {
				if !isHunkHeader(lines[index]) {
					return Document{}, fmt.Errorf("%w: update line %d must start a hunk", ErrInvalid, index+1)
				}

				index++
				hunk := Hunk{}

				for index < len(lines)-1 && !isFileHeader(lines[index]) && !isHunkHeader(lines[index]) {
					if lines[index] == "" || !strings.ContainsRune(" +-", rune(lines[index][0])) {
						return Document{}, fmt.Errorf("%w: hunk line %d must start with space, +, or -", ErrInvalid, index+1)
					}

					hunk.Lines = append(hunk.Lines, Line{
						Operation: lines[index][0],
						Text:      lines[index][1:],
					})
					index++
				}

				if len(hunk.Lines) == 0 {
					return Document{}, fmt.Errorf("%w: empty hunk for %q", ErrInvalid, name)
				}

				change.Hunks = append(change.Hunks, hunk)
			}

			if len(change.Hunks) == 0 {
				return Document{}, fmt.Errorf("%w: update %q has no hunks", ErrInvalid, name)
			}
		}

		document.Changes = append(document.Changes, change)
	}

	if len(document.Changes) == 0 {
		return Document{}, fmt.Errorf("%w: no file operations", ErrInvalid)
	}

	return document, nil
}

// Apply applies exact, ordered hunks to UTF-8 text in memory. It preserves a
// UTF-8 BOM, the existing line-ending style, and final-newline presence.
//
//nolint:gocyclo // Exact hunk application keeps conflict and encoding checks in one auditable state transition.
func Apply(original []byte, hunks []Hunk) ([]byte, error) {
	if bytes.IndexByte(original, 0) >= 0 || !utf8.Valid(original) {
		return nil, ErrBinary
	}

	bom := bytes.HasPrefix(original, []byte{0xef, 0xbb, 0xbf})

	text := original
	if bom {
		text = text[3:]
	}

	lineEnding, err := lineEnding(text)
	if err != nil {
		return nil, err
	}

	finalNewline := len(text) > 0 && bytes.HasSuffix(text, []byte(lineEnding))
	working := splitLines(string(text), lineEnding, finalNewline)
	cursor := 0

	for hunkIndex, hunk := range hunks {
		oldLines, newLines := hunkLines(hunk)
		if len(oldLines) == 0 {
			if len(working) != 0 || len(hunks) != 1 || hunkIndex != 0 {
				return nil, fmt.Errorf("%w: add-only hunk is ambiguous", ErrConflict)
			}

			working = append([]string(nil), newLines...)
			cursor = len(working)

			continue
		}

		matches := findMatches(working, oldLines, cursor)
		if len(matches) != 1 {
			return nil, fmt.Errorf(
				"%w: hunk %d matched %d locations",
				ErrConflict,
				hunkIndex+1,
				len(matches),
			)
		}

		position := matches[0]
		replacement := make([]string, 0, len(working)-len(oldLines)+len(newLines))
		replacement = append(replacement, working[:position]...)
		replacement = append(replacement, newLines...)
		replacement = append(replacement, working[position+len(oldLines):]...)
		working = replacement
		cursor = position + len(newLines)
	}

	updated := strings.Join(working, lineEnding)
	if finalNewline && len(working) > 0 {
		updated += lineEnding
	}

	if bom {
		return append([]byte{0xef, 0xbb, 0xbf}, []byte(updated)...), nil
	}

	return []byte(updated), nil
}

func parseHeader(line string) (Kind, string, bool) {
	for _, candidate := range []struct {
		prefix string
		kind   Kind
	}{
		{prefix: "*** Add File: ", kind: Add},
		{prefix: "*** Update File: ", kind: Update},
		{prefix: "*** Delete File: ", kind: Delete},
	} {
		if after, ok := strings.CutPrefix(line, candidate.prefix); ok {
			return candidate.kind, after, true
		}
	}

	return 0, "", false
}

func isFileHeader(line string) bool {
	_, _, ok := parseHeader(line)
	return ok
}

func isHunkHeader(line string) bool {
	return line == "@@" || strings.HasPrefix(line, "@@ ")
}

func lineEnding(value []byte) (string, error) {
	crlf := bytes.Count(value, []byte("\r\n"))

	lf := bytes.Count(value, []byte("\n")) - crlf
	if crlf > 0 && lf > 0 {
		return "", fmt.Errorf("%w: mixed line endings", ErrConflict)
	}

	if crlf > 0 {
		return "\r\n", nil
	}

	return "\n", nil
}

func splitLines(value, ending string, finalNewline bool) []string {
	if value == "" {
		return nil
	}

	lines := strings.Split(value, ending)
	if finalNewline {
		lines = lines[:len(lines)-1]
	}

	return lines
}

func hunkLines(hunk Hunk) ([]string, []string) {
	var (
		oldLines []string
		newLines []string
	)

	for _, line := range hunk.Lines {
		if line.Operation != '+' {
			oldLines = append(oldLines, line.Text)
		}

		if line.Operation != '-' {
			newLines = append(newLines, line.Text)
		}
	}

	return oldLines, newLines
}

func findMatches(value, pattern []string, start int) []int {
	if len(pattern) > len(value) || start > len(value)-len(pattern) {
		return nil
	}

	var matches []int

	for index := start; index <= len(value)-len(pattern); index++ {
		if slicesEqual(value[index:index+len(pattern)], pattern) {
			matches = append(matches, index)
		}
	}

	return matches
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}

	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}

	return true
}
