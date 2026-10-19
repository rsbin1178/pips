package coding

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEventMediaReferenceRoundTrip pins the strict codec contract for a
// persisted media reference: the new fields survive MarshalEvent/UnmarshalEvent
// and the inline `data` field is absent.
func TestEventMediaReferenceRoundTrip(t *testing.T) {
	t.Parallel()

	event := newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.User(ai.ImagePart{
		Source: ai.MediaSource{
			Path:     "assets/image-0123456789abcdef.jpg",
			Digest:   strings.Repeat("a", 64),
			MIMEType: "image/jpeg",
			Width:    1024,
			Height:   768,
		},
	})})

	require.NoError(t, ValidateEvent(event))

	encoded, err := MarshalEvent(event)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"path":"assets/image-0123456789abcdef.jpg"`)
	assert.NotContains(t, string(encoded), `"data"`)

	decoded, err := UnmarshalEvent(encoded)
	require.NoError(t, err)
	assert.Equal(t, event, decoded)
}

// TestValidateEventRejectsInvalidMediaReferences pins the one-of-source and
// reference-shape rules the strict codec must enforce.
func TestValidateEventRejectsInvalidMediaReferences(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("a", 64)
	image := func(source ai.MediaSource) Event {
		return newTestEvent(EventMessageCommitted, MessageCommitted{
			Message: ai.User(ai.ImagePart{Source: source}),
		})
	}

	tests := []struct {
		name  string
		event Event
	}{
		{
			name:  "path without digest",
			event: image(ai.MediaSource{Path: "assets/image-0123456789abcdef.png"}),
		},
		{
			name:  "digest too short",
			event: image(ai.MediaSource{Path: "assets/image-0123456789abcdef.png", Digest: "abcd"}),
		},
		{
			name: "data and path together",
			event: image(ai.MediaSource{
				Path: "assets/image-0123456789abcdef.png", Digest: digest, Data: []byte{1},
			}),
		},
		{
			name:  "path traversal",
			event: image(ai.MediaSource{Path: "../escape.png", Digest: digest}),
		},
		{
			name:  "unknown media directory",
			event: image(ai.MediaSource{Path: "other/image-0123456789abcdef.png", Digest: digest}),
		},
		{
			name:  "nested path component",
			event: image(ai.MediaSource{Path: "images/nested/image-0123456789abcdef.png", Digest: digest}),
		},
		{
			name:  "negative width",
			event: image(ai.MediaSource{Path: "assets/image-0123456789abcdef.png", Digest: digest, Width: -1}),
		},
		{
			name:  "negative height",
			event: image(ai.MediaSource{Path: "assets/image-0123456789abcdef.png", Digest: digest, Height: -1}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.ErrorIs(t, ValidateEvent(test.event), ErrInvalidEvent)
		})
	}
}

// TestUnmarshalEventRejectsMediaReferenceWithUnknownField proves the strict
// decoder rejects a field the reference schema does not define.
func TestUnmarshalEventRejectsMediaReferenceWithUnknownField(t *testing.T) {
	t.Parallel()

	event := newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.User(ai.ImagePart{
		Source: ai.MediaSource{
			Path: "assets/image-0123456789abcdef.png", Digest: strings.Repeat("a", 64),
		},
	})})

	encoded, err := MarshalEvent(event)
	require.NoError(t, err)

	tampered := strings.Replace(
		string(encoded),
		`"path":"assets/image-0123456789abcdef.png"`,
		`"path":"assets/image-0123456789abcdef.png","unknown":true`,
		1,
	)
	_, err = UnmarshalEvent([]byte(tampered))
	require.ErrorIs(t, err, ErrInvalidEvent)
}
