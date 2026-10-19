//go:build darwin || linux

//nolint:wsl_v5 // PTY launch, frame reads and teardown follow physical order.
package tui

import (
	"context"
	"image/color"
	"os"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/attachment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two behaviours below are only observable in the real TUI, so they are read
// back from a PTY frame rather than from the projection that feeds it.
const (
	imageRefusalPTYEnv  = "PIPS_TUI_IMAGE_REFUSAL_PTY_HELPER"
	fileCollapsePTYEnv  = "PIPS_TUI_FILE_COLLAPSE_PTY_HELPER"
	imageRefusalPTYTest = "TestPTYComposerImageWithoutVisionExplainsItself"
	fileCollapsePTYTest = "TestPTYTranscriptShowsTheFileReferenceNotItsContent"
)

// A pasted image on a model that cannot take one must say so where the operator is
// looking. Before this, the draft kept the image and Enter did nothing at all,
// which reads as a frozen TUI.
func TestPTYComposerImageWithoutVisionExplainsItself(t *testing.T) {
	if os.Getenv(imageRefusalPTYEnv) == "1" {
		runImageRefusalHelper(t)

		return
	}
	t.Parallel()

	harness := newPTYHarness(t, imageRefusalPTYEnv, imageRefusalPTYTest, 24, 80)

	require.Eventually(t, func() bool {
		return containsAll(harness.frame(80, 24), "Test Model")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	harness.write("\x16") // Ctrl+V reads the clipboard image

	require.Eventually(t, func() bool {
		return containsAll(harness.frame(80, 24),
			"[Image #1 · clipboard.png]", "does not accept image input")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	frame := harness.frame(80, 24)
	assert.Contains(t, frame, "[Image #1 · clipboard.png]", "the image is kept, never dropped")
	assert.Contains(t, frame, "does not accept image input", "and the reason is on screen")
	t.Logf("refusal frame:\n%s", frame)

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
}

func runImageRefusalHelper(t *testing.T) {
	t.Helper()

	controller, ok := reasoningController(t, reasoningModelConfig(), nil).(stubController)
	require.True(t, ok)
	// The model reports no image input, which is what the refusal is about.
	controller.capabilities = ai.Capabilities{Text: true, Tools: true}

	err := Run(context.Background(), Options{
		Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
		Workspace: t.TempDir(), Trusted: true, NoColor: true,
		PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
		MouseReporting: true,
		Clipboard: &clipboardStub{
			data: composerPNGFixture(t, color.RGBA{R: 8, G: 16, B: 32, A: 255}),
		},
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return controller, nil
		},
	})
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}

	os.Exit(0)
}

// The transcript is the reader's view of an attached file: the reference and its
// size, never the payload the model receives.
func TestPTYTranscriptShowsTheFileReferenceNotItsContent(t *testing.T) {
	if os.Getenv(fileCollapsePTYEnv) == "1" {
		runFileCollapseHelper(t)

		return
	}
	t.Parallel()

	harness := newPTYHarness(t, fileCollapsePTYEnv, fileCollapsePTYTest, 24, 80)

	require.Eventually(t, func() bool {
		return containsAll(harness.frame(80, 24), fileReferenceLabel)
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	frame := harness.frame(80, 24)
	assert.Contains(t, frame, fileReferenceLabel)
	assert.NotContains(t, frame, fileReferenceMarker, "the payload stays out of the transcript")
	t.Logf("collapsed frame:\n%s", frame)

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
}

const (
	fileReferenceLabel  = "[File · src/main.go · 2 lines]"
	fileReferenceMarker = "FILE-CONTENT-MARKER"
)

func runFileCollapseHelper(t *testing.T) {
	t.Helper()

	controller, ok := reasoningController(t, reasoningModelConfig(), nil).(stubController)
	require.True(t, ok)

	// The message is assembled the way the Composer assembles one — the operator's
	// runs around a provenance part of its own — so the frame under test is the one
	// a real @ reference produces.
	text := attachment.Text{
		Reference: attachment.Reference{Path: "src/main.go", Kind: attachment.KindText},
		Content:   fileReferenceMarker + "-ONE\n" + fileReferenceMarker + "-TWO\n",
	}
	state := readyState()
	state.Transcript = []ai.Message{
		ai.User(ai.Text("before "), ai.Text(text.PromptText()), ai.Text(" after")),
	}
	controller.state = state

	err := Run(context.Background(), Options{
		Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
		Workspace: t.TempDir(), Trusted: true, NoColor: true,
		PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
		MouseReporting: true,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return controller, nil
		},
	})
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}

	os.Exit(0)
}
