//nolint:wsl_v5 // Rendering remains a bounded sequential projection of canonical records.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
)

type archiveRenderer struct {
	writer      io.Writer
	buffer      strings.Builder
	segments    []archiveSegment
	total       int64
	lines       int
	first, last int
}

func (r *archiveRenderer) entry(ctx context.Context, record int, entry harness.Entry, canonical []byte) error {
	kind := string(entry.Kind)
	if entry.Message != nil {
		kind += " / " + archiveMessageRole(entry.Message)
	}
	header := fmt.Sprintf("Record %d | %s | %s\n", record, entry.ID, kind)
	if err := r.text(ctx, record, header); err != nil {
		return err
	}
	if entry.Message == nil {
		// Includes custom records and newer checkpoint variants without interpreting
		// their references as authority or depending on a particular checkpoint API.
		return r.text(ctx, record, string(canonical))
	}
	parts, err := ai.MessageParts(entry.Message)
	if err != nil {
		return err
	}
	slices.Reverse(parts)
	for len(parts) > 0 {
		part := parts[len(parts)-1]
		parts = parts[:len(parts)-1]
		text, err := archivePartText(part)
		if err != nil {
			return err
		}
		if result, ok := part.(ai.ToolResultPart); ok {
			for _, child := range slices.Backward(result.Content) {
				parts = append(parts, child)
			}
		}
		if err := r.text(ctx, record, text); err != nil {
			return err
		}
	}
	return nil
}

//nolint:goconst // Role names appear once here; coincidentally equal test entry IDs are unrelated.
func archiveMessageRole(message ai.Message) string {
	switch message.(type) {
	case ai.SystemMessage:
		return "system"
	case ai.UserMessage:
		return "user"
	case ai.AssistantMessage:
		return "assistant"
	case ai.ToolMessage:
		return "tool"
	default:
		return "unknown"
	}
}

func archivePartText(part ai.Part) (string, error) {
	switch value := part.(type) {
	case ai.TextPart:
		return value.Text, nil
	case ai.ReasoningPart:
		return "[historical reasoning]\n" + value.Text, nil
	case ai.ToolCallPart:
		return "Tool call " + value.Name + " (" + value.ID + ")\n" + string(value.Args), nil
	case ai.ToolResultPart:
		return fmt.Sprintf("Tool result %s (%s), error=%t", value.Name, value.ToolCallID, value.IsError), nil
	case ai.StructuredContentPart:
		return string(value.Data), nil
	case ai.ResourceLinkPart:
		return "Resource " + value.URI + "\n" + value.Name + "\n" + value.Title + "\n" + value.Description, nil
	case ai.EmbeddedResourcePart:
		return "Embedded resource " + value.URI + "\n" + value.Text, nil
	case ai.ImagePart:
		return "[image retained in canonical record]", nil
	case ai.FilePart:
		return "[file retained in canonical record] " + value.Name, nil
	default:
		return "", ErrArchiveInvalid
	}
}

// Long logical lines are wrapped only in the searchable projection. Canonical
// JSONL is untouched. Segment first/last record indexes identify continuations.
func (r *archiveRenderer) text(ctx context.Context, record int, text string) error {
	if !utf8.ValidString(text) {
		return ErrArchiveInvalid
	}
	for text != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Search only the next bounded line, not the entire remaining payload:
		// repeated full-suffix scans would be quadratic for large Tool results.
		line, rest := text, ""
		if newline := strings.IndexByte(text[:min(len(text), archiveLineBytes+1)], '\n'); newline >= 0 {
			line, rest = text[:newline], text[newline+1:]
		} else if len(text) > archiveLineBytes {
			end := archiveLineBytes
			for !utf8.RuneStart(text[end]) {
				end--
			}
			line, rest = text[:end], text[end:]
		}
		if err := r.line(record, line); err != nil {
			return err
		}
		text = rest
	}
	return nil
}

func (r *archiveRenderer) line(record int, line string) error {
	if r.total+int64(len(line)+1) > ArchiveMaxRenderedBytes {
		return ErrArchiveLimit
	}
	if r.buffer.Len()+len(line)+1 > archiveSegmentBytes {
		if err := r.flush(); err != nil {
			return err
		}
	}
	if r.first == 0 {
		r.first = record
	}
	r.last = record
	r.buffer.WriteString(line)
	r.buffer.WriteByte('\n')
	r.lines++
	r.total += int64(len(line) + 1)
	return nil
}

func (r *archiveRenderer) flush() error {
	if r.buffer.Len() == 0 {
		return nil
	}
	if len(r.segments) >= ArchiveMaxSegments {
		return ErrArchiveLimit
	}
	text := r.buffer.String()
	if _, err := io.WriteString(r.writer, text); err != nil {
		return archiveStorageError(err)
	}
	hash := sha256.Sum256([]byte(text))
	r.segments = append(r.segments, archiveSegment{
		ID: fmt.Sprintf("seg-%06d", len(r.segments)+1), Bytes: int64(len(text)), SHA256: hex.EncodeToString(hash[:]),
		Lines: r.lines, FirstRecord: r.first, LastRecord: r.last,
	})
	r.buffer.Reset()
	r.lines, r.first, r.last = 0, 0, 0
	return nil
}
