package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/rsbin/pips/internal/coding/workspace"
)

type workspaceFileResolver func(
	context.Context,
	attachment.Reference,
) (attachment.Text, error)

func resolveComposerSnapshot(
	ctx context.Context,
	snapshot composerSnapshot,
	resolve workspaceFileResolver,
) (ai.Message, error) {
	if !validComposerSnapshot(snapshot) {
		return ai.Message{}, errComposerCorruptDraft
	}

	if resolve == nil && composerSnapshotHasFiles(snapshot) {
		return ai.Message{}, errors.New("coding tui: resolve Composer: missing file resolver")
	}

	builder := newComposerMessageBuilder(len(snapshot.elements)*2 + 1)
	spans := composerElementSpans(snapshot)
	last := 0

	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return ai.Message{}, err
		}

		if err := builder.appendRun(snapshot.display[last:span.start]); err != nil {
			return ai.Message{}, err
		}

		element := snapshot.elements[span.elementIndex]
		if err := appendComposerElement(ctx, builder, element, resolve); err != nil {
			return ai.Message{}, err
		}

		last = span.end
	}

	if err := builder.appendRun(snapshot.display[last:]); err != nil {
		return ai.Message{}, err
	}

	return builder.message()
}

func appendComposerElement(
	ctx context.Context,
	builder *composerMessageBuilder,
	element composerElement,
	resolve workspaceFileResolver,
) error {
	switch element.kind {
	case composerElementPaste:
		return builder.appendRun(element.payload)
	case composerElementFile:
		builder.flushRun()

		resolved, err := resolve(ctx, element.file)
		if err != nil {
			return fmt.Errorf(
				"coding tui: resolve Workspace file %q: %w",
				element.file.Path,
				err,
			)
		}

		if resolved.Reference != element.file {
			return fmt.Errorf(
				"%w: Workspace file resolver returned a different reference",
				workspace.ErrChanged,
			)
		}

		return builder.appendPart(resolved.PromptText())
	default:
		return errComposerCorruptDraft
	}
}

type composerMessageBuilder struct {
	parts     []ai.Part
	textParts []string
	run       strings.Builder
	total     int
}

func newComposerMessageBuilder(capacity int) *composerMessageBuilder {
	return &composerMessageBuilder{
		parts: make([]ai.Part, 0, capacity), textParts: make([]string, 0, capacity),
	}
}

func (b *composerMessageBuilder) appendRun(value string) error {
	if err := b.reserve(value); err != nil {
		return err
	}

	b.run.WriteString(value)

	return nil
}

func (b *composerMessageBuilder) appendPart(value string) error {
	if err := b.reserve(value); err != nil {
		return err
	}

	b.parts = append(b.parts, ai.Text(value))
	b.textParts = append(b.textParts, value)

	return nil
}

func (b *composerMessageBuilder) reserve(value string) error {
	if len(value) > coding.MaxPromptTextBytes-b.total {
		return fmt.Errorf("coding tui: resolve Composer: %w", coding.ErrInvalidPrompt)
	}

	b.total += len(value)

	return nil
}

func (b *composerMessageBuilder) flushRun() {
	if b.run.Len() == 0 {
		return
	}

	value := b.run.String()
	b.parts = append(b.parts, ai.Text(value))
	b.textParts = append(b.textParts, value)
	b.run.Reset()
}

func (b *composerMessageBuilder) message() (ai.Message, error) {
	b.flushRun()

	assembled := strings.Join(b.textParts, "")
	if err := coding.ValidatePromptText(assembled); err != nil {
		return ai.Message{}, fmt.Errorf("coding tui: resolve Composer: %w", err)
	}

	return ai.User(b.parts...), nil
}
