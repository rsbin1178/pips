package coding

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/media"
	"github.com/rsbin1178/pips/internal/coding/session"
)

// sessionMediaStore opens the media store of one session directory. It is the
// single writer seam for user media: clipboard, Workspace (@), and ACP images
// all arrive here as inline user parts and leave as content-addressed
// references.
func (r *Runtime) sessionMediaStore() (*session.MediaStore, error) {
	if r == nil || r.handle == nil {
		return nil, nil
	}

	return session.NewMediaStore(r.handle.Dir())
}

// mediaHydrator builds the request-assembly transform of one Runtime: it
// resolves session media references to verified bytes and fits the request to
// the configured budget. Both the primary Agent and subagents install it.
func (r *Runtime) mediaHydrator() *media.Hydrator {
	store, err := r.sessionMediaStore()
	if err != nil || store == nil {
		return nil
	}

	return media.New(mediaLoader{store: store}, r.config.Media.MaxRequestBytes)
}

// mediaLoader adapts a session media store to the hydrator's Loader seam.
type mediaLoader struct {
	store *session.MediaStore
}

// childMediaHydrator builds a subagent's request transform over its own child
// session directory, sharing the conversation's byte budget.
func childMediaHydrator(handle *session.Handle, budget int) *media.Hydrator {
	if handle == nil {
		return nil
	}

	store, err := session.NewMediaStore(handle.Dir())
	if err != nil {
		return nil
	}

	return media.New(mediaLoader{store: store}, budget)
}

// pruneSessionMedia is the on-open maintenance pass: it removes media this
// session's own references no longer name and that is older than the TTL. It is
// best-effort — an unreadable reference skips the pass rather than failing
// Open — and a zero TTL disables it.
func (r *Runtime) pruneSessionMedia(ctx context.Context) {
	if r == nil || r.handle == nil || r.config.Media.CleanupTTLDays <= 0 {
		return
	}

	age := time.Duration(r.config.Media.CleanupTTLDays) * 24 * time.Hour
	_, _ = session.PruneSessionMedia(context.WithoutCancel(ctx), r.handle.Dir(), age, false)
}

// queueUserMedia persists inline user media on messages queued mid-run through
// Steer, FollowUp, or Interject, so those paths store a session-relative
// reference exactly like Prompt does. Text-only and already-referenced messages
// pass through unchanged.
func (r *Runtime) queueUserMedia(messages []ai.Message) ([]ai.Message, error) {
	store, err := r.sessionMediaStore()
	if err != nil {
		return nil, err
	}

	return persistUserMedia(context.Background(), store, messages)
}

func (l mediaLoader) Load(ctx context.Context, path, digest string) ([]byte, error) {
	ref, err := session.MediaRefFromSource(path, digest, "", 0, 0)
	if err != nil {
		return nil, err
	}

	return l.store.Open(ctx, ref)
}

// persistUserMedia writes every inline user image to the session media store
// and returns the messages with a session-relative reference in place of the
// inline bytes. Messages without inline user images are returned unchanged, so
// an already-referenced or text-only prompt costs nothing.
func persistUserMedia(
	ctx context.Context,
	store *session.MediaStore,
	messages []ai.Message,
) ([]ai.Message, error) {
	if store == nil {
		return messages, nil
	}

	changed := false
	out := make([]ai.Message, len(messages))
	copy(out, messages)

	for index, message := range messages {
		user, ok := message.(ai.UserMessage)
		if !ok {
			continue
		}

		parts, partChanged, err := persistUserParts(ctx, store, user.Parts)
		if err != nil {
			return nil, err
		}

		if partChanged {
			out[index] = ai.UserMessage{Parts: parts}
			changed = true
		}
	}

	if !changed {
		return messages, nil
	}

	return out, nil
}

func persistUserParts(
	ctx context.Context,
	store *session.MediaStore,
	parts []ai.UserPart,
) ([]ai.UserPart, bool, error) {
	changed := false
	out := make([]ai.UserPart, len(parts))
	copy(out, parts)

	for index, part := range parts {
		image, ok := part.(ai.ImagePart)
		if !ok || image.Source.Path != "" || len(image.Source.Data) == 0 {
			continue
		}

		original, err := store.PutOriginal(ctx, image.Source.MIMEType, image.Source.Data)
		if err != nil {
			return nil, false, fmt.Errorf("coding runtime: persist user image: %w", err)
		}

		copyRef, err := store.PutModelCopy(ctx, original, image.Source.MIMEType, image.Source.Data)
		if err != nil {
			return nil, false, fmt.Errorf("coding runtime: persist user image: %w", err)
		}

		width, height := image.Source.Width, image.Source.Height
		if width <= 0 || height <= 0 {
			width, height = imageDimensions(image.Source.Data)
		}

		out[index] = ai.ImagePart{Source: ai.MediaSource{
			Path:     copyRef.Path,
			Digest:   copyRef.Digest,
			MIMEType: copyRef.MIMEType,
			Width:    width,
			Height:   height,
		}}
		changed = true
	}

	return out, changed, nil
}

// imageDimensions reads only the encoded image header, so a reference carries
// its pixel size without a full decode. Unknown formats report 0×0.
func imageDimensions(data []byte) (int, int) {
	configuration, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}

	return configuration.Width, configuration.Height
}
