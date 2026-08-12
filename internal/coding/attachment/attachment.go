// Package attachment owns bounded Workspace attachment discovery and resolution.
package attachment

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const (
	// MaxTextBytes is the largest Workspace text file accepted as prompt context.
	MaxTextBytes = 512 << 10
	// MaxPathBytes bounds Workspace-relative attachment paths and provenance.
	MaxPathBytes = 4 << 10
	// MaxEncodedImageBytes is the largest encoded image accepted at ingress.
	MaxEncodedImageBytes = 20 << 20
	// MaxImageBytes is the largest normalized inline image accepted by Coding.
	MaxImageBytes = 1 << 20
	// MaxImagePixels bounds decoded image work before a full decode is attempted.
	MaxImagePixels = 40_000_000
	// MaxImagesPerMessage bounds images in one assembled user message.
	MaxImagesPerMessage = 4
	// MaxImageBytesPerMessage bounds aggregate inline image data in one message.
	MaxImageBytesPerMessage = 2 << 20
)

var (
	// ErrDenied means a path enters private VCS metadata.
	ErrDenied = errors.New("coding attachment: path denied")
	// ErrLimit means a discovery or text-content boundary was exceeded.
	ErrLimit = errors.New("coding attachment: limit exceeded")
	// ErrBinaryText means a selected text file is not valid NUL-free UTF-8.
	ErrBinaryText = errors.New("coding attachment: binary text file")
	// ErrInvalidImage means encoded image data or metadata is malformed.
	ErrInvalidImage = errors.New("coding attachment: invalid image")
	// ErrUnsupportedImage means the encoded image format is not accepted.
	ErrUnsupportedImage = errors.New("coding attachment: unsupported image format")
)

// Kind classifies a Workspace attachment without reading its content eagerly.
type Kind uint8

// Attachment kinds.
const (
	KindUnknown Kind = iota
	KindText
	KindImage
)

// Summary is one content-free Workspace file discovery result.
type Summary struct {
	Path string
	Kind Kind
	Size int64
}

// Reference is the immutable path and kind retained by a Composer element.
type Reference struct {
	Path string
	Kind Kind
}

// Snapshot is one bounded, detached discovery result.
type Snapshot struct {
	Files     []Summary
	Truncated bool
}

// Clone returns a detached discovery snapshot.
func (s Snapshot) Clone() Snapshot {
	s.Files = slices.Clone(s.Files)

	return s
}

// Reference returns the immutable Composer reference for a summary.
func (s Summary) Reference() Reference {
	return Reference{Path: s.Path, Kind: s.Kind}
}

// Text is one final-handle-resolved Workspace text attachment.
type Text struct {
	Reference Reference
	Content   string
}

// PromptText returns a provider-neutral text part with bounded provenance.
func (t Text) PromptText() string {
	return fmt.Sprintf("\n\n[Workspace file: %s]\n%s", t.Reference.Path, t.Content)
}

// NormalizeReference validates a Workspace-relative reference, denies private
// VCS metadata, and derives its kind from the normalized path.
func NormalizeReference(reference Reference) (Reference, error) {
	normalized, err := workspace.NormalizePath(reference.Path, false)
	if err != nil {
		return Reference{}, err
	}

	if len(normalized) > MaxPathBytes || !utf8.ValidString(normalized) {
		return Reference{}, fmt.Errorf("%w: invalid path length", workspace.ErrInvalidPath)
	}

	for _, character := range normalized {
		if unicode.IsControl(character) {
			return Reference{}, fmt.Errorf("%w: control-bearing path", workspace.ErrInvalidPath)
		}
	}

	if deniedPath(normalized) {
		return Reference{}, fmt.Errorf("%w: %q", ErrDenied, normalized)
	}

	kind := kindForPath(normalized)
	if reference.Kind != KindUnknown && reference.Kind != kind {
		return Reference{}, fmt.Errorf("%w: attachment kind changed", workspace.ErrChanged)
	}

	return Reference{Path: normalized, Kind: kind}, nil
}

func deniedPath(name string) bool {
	for segment := range strings.SplitSeq(name, "/") {
		lower := strings.ToLower(segment)
		if lower == ".git" {
			return true
		}
	}

	return false
}

func kindForPath(name string) Kind {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return KindImage
	default:
		return KindText
	}
}
