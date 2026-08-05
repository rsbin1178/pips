//nolint:wsl_v5 // Picker actions and Composer resolution assertions stay locally grouped.
package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAtPickerSelectsProtectedWorkspaceFile(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	controller.files = attachment.Snapshot{Files: []attachment.Summary{
		{Path: "src/domain.go", Kind: attachment.KindText, Size: 10},
		{Path: "cmd/main.go", Kind: attachment.KindText, Size: 20},
		{Path: "assets/main.png", Kind: attachment.KindImage, Size: 30},
	}}
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("review ")

	_, load := model.Update(tea.KeyPressMsg{Text: "@"})
	require.NotNil(t, load)
	model.Update(commandMessage(t, load))
	assert.Equal(t, pickerFile, model.picker.kind)
	assert.Equal(t, "review @", model.composer.Value())

	for _, character := range "main.go" {
		model.Update(tea.KeyPressMsg{Text: string(character)})
	}
	require.NotEmpty(t, model.filteredFiles())
	assert.Equal(t, "cmd/main.go", model.filteredFiles()[0].Path)

	model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Equal(t, pickerNone, model.picker.kind)
	require.Len(t, model.composer.elements, 1)
	assert.Equal(t, attachment.Reference{
		Path: "cmd/main.go", Kind: attachment.KindText,
	}, model.composer.elements[0].file)
	assert.Equal(t, "review [File #1 · cmd/main.go] ", model.composer.Value())
}

func TestAtPickerCancelRestoresExactRichDraft(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	controller.files = attachment.Snapshot{Files: []attachment.Summary{{
		Path: "file.go", Kind: attachment.KindText,
	}}}
	model := readyModelWithController(t, controller, true)
	model.composer.InsertString("before ")
	_, err := model.composer.InsertPaste(strings.Repeat("payload\n", 9))
	require.NoError(t, err)
	model.composer.InsertString(" after ")
	before := model.composer.Snapshot()

	_, load := model.Update(tea.KeyPressMsg{Text: "@"})
	require.NotNil(t, load)
	model.Update(commandMessage(t, load))
	model.Update(tea.KeyPressMsg{Text: "file"})
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, before, model.composer.Snapshot())
}

func TestAtPickerOnlyOpensForTypedTokenBoundary(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	model := readyModelWithController(t, controller, true)

	model.Update(tea.PasteMsg{Content: "@file.go"})
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, "@file.go", model.composer.Value())

	model.composer.Reset()
	model.composer.InsertString("name")
	_, load := model.Update(tea.KeyPressMsg{Text: "@"})
	assert.Nil(t, load)
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, "name@", model.composer.Value())

	model.composer.Reset()
	model.composer.InsertString("attach ")
	_, load = model.Update(tea.KeyPressMsg{Text: "@"})
	require.NotNil(t, load)
	assert.Equal(t, pickerFile, model.picker.kind)
}

func TestFilePickerRankingIsDeterministic(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.picker = pickerState{
		kind:  pickerFile,
		query: "main",
		files: []attachment.Summary{
			{Path: "src/migration.go"},
			{Path: "folder/main/readme.md"},
			{Path: "z/main.go"},
			{Path: "a/domain.go"},
		},
	}

	filtered := model.filteredFiles()
	paths := make([]string, len(filtered))
	for index := range filtered {
		paths[index] = filtered[index].Path
	}
	assert.Equal(t, []string{
		"z/main.go",
		"a/domain.go",
		"folder/main/readme.md",
		"src/migration.go",
	}, paths)
}

func TestFilePickerIgnoresStaleLoadAndRendersNarrowNoColor(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 24, Height: 12})
	model.picker = pickerState{kind: pickerFile, generation: 2, loading: true}
	model.Update(filePickerDataMsg{
		generation: 1,
		snapshot:   attachment.Snapshot{Files: []attachment.Summary{{Path: "stale.go"}}},
	})
	assert.Empty(t, model.picker.files)
	assert.True(t, model.picker.loading)

	model.Update(filePickerDataMsg{
		generation: 2,
		snapshot: attachment.Snapshot{
			Files: []attachment.Summary{{
				Path: "very/long/workspace/path/file.go", Kind: attachment.KindText, Size: 42,
			}},
			Truncated: true,
		},
	})
	view := model.filePickerView(4)
	assert.NotContains(t, view, "\x1b[")
	assert.Contains(t, view, "Limited results")
	for line := range strings.SplitSeq(view, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), model.width)
	}
}

func TestComposerResolvesWorkspaceFilesInVisibleOrder(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	controller.resolve = func(
		_ context.Context,
		reference attachment.Reference,
	) (attachment.Resolved, error) {
		return attachment.NewResolvedText(attachment.Text{
			Reference: reference,
			Content:   "file contents",
		})
	}
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("before @file")
	require.NoError(t, model.composer.InsertFile(
		len("before "),
		len("before @file"),
		attachment.Reference{Path: "docs/file.txt"},
	))
	model.composer.InsertString(" after ")
	paste := strings.Repeat("pasted line\n", 9)
	_, err := model.composer.InsertPaste(paste)
	require.NoError(t, err)
	before := model.composer.Snapshot()

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	assert.True(t, model.composerResolving)
	driveModelCommands(t, model, command)

	require.Len(t, controller.prompts, 1)
	require.Len(t, controller.prompts[0].Parts, 3)
	assert.Equal(t, "before ", textPart(t, controller.prompts[0].Parts[0]))
	assert.Equal(
		t,
		"\n\n[Workspace file: docs/file.txt]\nfile contents",
		textPart(t, controller.prompts[0].Parts[1]),
	)
	assert.Equal(t, " after "+paste, textPart(t, controller.prompts[0].Parts[2]))
	assert.Empty(t, model.composer.Value())
	require.Len(t, model.composer.history, 1)
	assert.True(t, equalComposerStructure(before, model.composer.history[0]))
}

func TestComposerResolutionFailurePreservesExactDraft(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	controller.resolve = func(
		context.Context,
		attachment.Reference,
	) (attachment.Resolved, error) {
		return attachment.Resolved{}, workspace.ErrChanged
	}
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("@file")
	require.NoError(t, model.composer.InsertFile(
		0,
		len("@file"),
		attachment.Reference{Path: "file.txt"},
	))
	before := model.composer.Snapshot()

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	model.Update(command())

	assert.Equal(t, before, model.composer.Snapshot())
	require.ErrorIs(t, model.streamErr, workspace.ErrChanged)
	assert.False(t, model.composerResolving)
	assert.Empty(t, controller.prompts)
}

func TestComposerResolutionRejectsTotalTextOverage(t *testing.T) {
	t.Parallel()

	composer := newTestComposer()
	composer.SetValue("@one @two")
	require.NoError(t, composer.InsertFile(
		0,
		len("@one"),
		attachment.Reference{Path: "one.txt"},
	))
	start := strings.Index(composer.Value(), "@two")
	require.NotEqual(t, -1, start)
	require.NoError(t, composer.InsertFile(
		start,
		start+len("@two"),
		attachment.Reference{Path: "two.txt"},
	))

	_, err := resolveComposerSnapshot(
		t.Context(),
		composer.Snapshot(),
		true,
		func(
			_ context.Context,
			reference attachment.Reference,
		) (attachment.Resolved, error) {
			return attachment.NewResolvedText(attachment.Text{
				Reference: reference,
				Content:   strings.Repeat("x", attachment.MaxTextBytes),
			})
		},
	)
	require.ErrorIs(t, err, coding.ErrInvalidPrompt)
}

func TestProtectedFileReferenceIsAtomic(t *testing.T) {
	t.Parallel()

	composer := newTestComposer()
	composer.SetValue("before @file after")
	require.NoError(t, composer.InsertFile(
		len("before "),
		len("before @file"),
		attachment.Reference{Path: "file.txt"},
	))
	require.Len(t, composer.elements, 1)
	label := composer.elements[0].label
	line, column := composerPositionAtByte(composer.Value(), len("before ")+len(label))
	setComposerPosition(&composer, line, column)

	composer, _ = composer.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	assert.Equal(t, "before  after", composer.Value())
	assert.Empty(t, composer.elements)
}

func TestImageReferenceVisionRefusalPreservesDraft(t *testing.T) {
	t.Parallel()

	controller := newWorkspaceAttachmentController(readyState())
	controller.capabilities.Vision = false
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("@logo")
	require.NoError(t, model.composer.InsertFile(
		0,
		len("@logo"),
		attachment.Reference{Path: "assets/logo.png", Kind: attachment.KindImage},
	))
	before := model.composer.Snapshot()

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, command)
	assert.Equal(t, before, model.composer.Snapshot())
	require.ErrorIs(t, model.streamErr, errComposerVisionUnsupported)
	assert.Empty(t, controller.prompts)
}

func TestComposerResolutionCanBeCanceledWithoutDraftLoss(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.composer.SetValue("keep [File #1 · file.txt]")
	model.composer.elements = []composerElement{{
		id: 1, kind: composerElementAttachment, label: "[File #1 · file.txt]",
		file: attachment.Reference{Path: "file.txt", Kind: attachment.KindText},
	}}
	before := model.composer.Snapshot()
	canceled := false
	model.composerResolving = true
	model.composerResolveSeq = 4
	model.composerCancel = func() { canceled = true }

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.True(t, canceled)
	assert.False(t, model.composerResolving)
	assert.Equal(t, before, model.composer.Snapshot())

	model.Update(composerResolvedMsg{generation: 4, err: assert.AnError})
	require.NoError(t, model.streamErr)
	assert.Equal(t, before, model.composer.Snapshot())
}

type workspaceAttachmentController struct {
	*overlayController
	files   attachment.Snapshot
	listErr error
	resolve workspaceFileResolver
}

func newWorkspaceAttachmentController(state coding.State) *workspaceAttachmentController {
	return &workspaceAttachmentController{overlayController: newOverlayController(state)}
}

func (c *workspaceAttachmentController) ListWorkspaceFiles(
	context.Context,
) (attachment.Snapshot, error) {
	return c.files.Clone(), c.listErr
}

func (c *workspaceAttachmentController) ResolveWorkspaceFile(
	ctx context.Context,
	reference attachment.Reference,
) (attachment.Resolved, error) {
	if c.resolve == nil {
		return attachment.Resolved{}, errors.New("unexpected Workspace file resolution")
	}

	return c.resolve(ctx, reference)
}

func textPart(t *testing.T, part ai.Part) string {
	t.Helper()

	text, ok := part.(ai.TextPart)
	require.True(t, ok)

	return text.Text
}
