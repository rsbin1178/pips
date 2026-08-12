package attachment

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// Resolved is one final-handle-resolved Workspace attachment. Exactly one
// content accessor succeeds according to Kind.
type Resolved struct {
	reference Reference
	text      Text
	image     Image
}

// NewResolvedText validates a detached text result for resolver adapters and
// tests. Production Workspace reads are created by Resolve.
func NewResolvedText(text Text) (Resolved, error) {
	normalized, err := NormalizeReference(text.Reference)
	if err != nil {
		return Resolved{}, err
	}

	if normalized.Kind != KindText || normalized != text.Reference ||
		len(text.Content) > MaxTextBytes || strings.IndexByte(text.Content, 0) >= 0 ||
		!utf8.ValidString(text.Content) {
		return Resolved{}, fmt.Errorf("%w: invalid resolved text", workspace.ErrChanged)
	}

	return Resolved{reference: normalized, text: text}, nil
}

// NewResolvedImage validates a normalized Workspace image result for resolver
// adapters and tests. Production Workspace reads are created by Resolve.
func NewResolvedImage(reference Reference, image Image) (Resolved, error) {
	normalized, err := NormalizeReference(reference)
	if err != nil {
		return Resolved{}, err
	}

	if normalized.Kind != KindImage || normalized != reference || !image.Valid() ||
		image.Name() != normalized.Path {
		return Resolved{}, fmt.Errorf("%w: invalid resolved image", workspace.ErrChanged)
	}

	return Resolved{reference: normalized, image: image}, nil
}

// Reference returns the normalized Workspace reference.
func (r Resolved) Reference() Reference { return r.reference }

// Kind returns the resolved content kind.
func (r Resolved) Kind() Kind {
	if r.reference.Kind == KindText && r.text.Reference == r.reference {
		return KindText
	}

	if r.reference.Kind == KindImage && r.image.Valid() {
		return KindImage
	}

	return KindUnknown
}

// Text returns the resolved text when Kind is KindText.
func (r Resolved) Text() (Text, bool) {
	if r.Kind() != KindText {
		return Text{}, false
	}

	return r.text, true
}

// Image returns the immutable normalized image when Kind is KindImage.
func (r Resolved) Image() (Image, bool) {
	if r.Kind() != KindImage {
		return Image{}, false
	}

	return r.image, true
}
