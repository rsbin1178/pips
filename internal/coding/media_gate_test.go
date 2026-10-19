package coding

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/extension"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaCapabilityGateCheck(t *testing.T) {
	t.Parallel()

	image := ai.ImageData("image/png", []byte{1, 2, 3})
	file := ai.FileData("doc.pdf", "application/pdf", []byte("%PDF"))

	structured, err := ai.StructuredContent(ai.JSON(`{"ok":true}`))
	require.NoError(t, err)

	link := ai.ResourceLinkPart{URI: "file:///tmp/report.csv"}
	embeddedImage := ai.EmbeddedResourcePart{URI: "mem://shot", MIMEType: "image/png", Blob: []byte{1}}

	tests := []struct {
		name        string
		gate        MediaCapabilityGate
		parts       []ai.Part
		wantRefused bool
		want        []ai.Part
	}{
		{
			name:  "vision accepts an image",
			gate:  MediaCapabilityGate{Vision: true},
			parts: []ai.Part{ai.Text("shot"), image},
			want:  []ai.Part{ai.Text("shot"), image},
		},
		{
			name:        "missing vision refuses an image",
			gate:        MediaCapabilityGate{},
			parts:       []ai.Part{ai.Text("shot"), image},
			wantRefused: true,
			want: []ai.Part{
				ai.Text("shot"),
				ai.Text(mediaRefusalText(mediaKindImage, "read")),
			},
		},
		{
			name:        "missing documents refuses a file",
			gate:        MediaCapabilityGate{Vision: true},
			parts:       []ai.Part{file},
			wantRefused: true,
			want: []ai.Part{
				ai.Text(mediaRefusalText(mediaKindFile, "read")),
			},
		},
		{
			name:  "documents accept a file",
			gate:  MediaCapabilityGate{Documents: true},
			parts: []ai.Part{file},
			want:  []ai.Part{file},
		},
		{
			name:        "embedded image blob follows vision",
			gate:        MediaCapabilityGate{Documents: true},
			parts:       []ai.Part{embeddedImage},
			wantRefused: true,
			want: []ai.Part{
				ai.Text(mediaRefusalText(mediaKindImage, "read")),
			},
		},
		{
			name:  "text and non-media kinds pass through",
			gate:  MediaCapabilityGate{},
			parts: []ai.Part{ai.Text("summary"), structured, link},
			want:  []ai.Part{ai.Text("summary"), structured, link},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, refused := tt.gate.Check("read", tt.parts)
			assert.Equal(t, tt.wantRefused, refused)
			assert.Equal(t, tt.want, got)
		})
	}
}

// visionRuntimeModel reports scripted capabilities so a test can switch what a
// tool result is allowed to carry.
type visionRuntimeModel struct {
	*runtimeModel
	capabilities ai.Capabilities
}

func (m *visionRuntimeModel) Capabilities() ai.Capabilities { return m.capabilities }

// TestMediaGateFollowsModelCapabilities is the model-switch case: two tool
// results pass through the gate under two different models, and only the
// vision-blind model refuses the image.
func TestMediaGateFollowsModelCapabilities(t *testing.T) {
	t.Parallel()

	vision := &visionRuntimeModel{newRuntimeModel(), ai.Capabilities{Text: true, Tools: true, Vision: true}}
	blind := &visionRuntimeModel{newRuntimeModel(), ai.Capabilities{Text: true, Tools: true}}

	content := []ai.Part{ai.ImageData("image/png", []byte{1, 2, 3})}

	var info agent.ToolResultInfo

	info.Name = "read"
	info.Result = ai.ToolResultPart{Content: content}

	accepted := mediaGateAfterTool(vision)(context.Background(), info)
	require.Nil(t, accepted, "a vision model keeps the image untouched")

	refused := mediaGateAfterTool(blind)(context.Background(), info)
	require.NotNil(t, refused)
	assert.Equal(t, []ai.Part{ai.Text(mediaRefusalText(mediaKindImage, "read"))}, refused.Content)
}

// TestRuntimeRefusesToolMediaTheModelCannotAccept drives the gate through a
// real interaction: a tool returns an image, the active model declares no
// vision, and the committed result and the provider request carry the refusal
// text instead of the image.
func TestRuntimeRefusesToolMediaTheModelCannotAccept(t *testing.T) {
	t.Parallel()

	imageTool := agent.NewToolParts("screenshot", "Capture the screen.", func(
		context.Context,
		struct{},
	) ([]ai.Part, error) {
		return []ai.Part{
			ai.Text("captured"),
			ai.ImageData("image/png", []byte{1, 2, 3}),
		}, nil
	})

	ext, err := extension.NewDefinition(extension.Descriptor{
		ID: "screenshot-tool", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{
			Tools: []extension.Tool{{Value: imageTool, Risk: catalog.RiskRead}},
		}, nil
	})
	require.NoError(t, err)

	model := newRuntimeModel(
		runtimeToolResponse("call-shot", "screenshot", `{}`),
		runtimeTextResponse("handled"),
	)
	runtime := openTestRuntimeWithExtensions(t, model, ext)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("screenshot")))

	contextValue, err := runtime.session.Context()
	require.NoError(t, err)

	content := toolResultContentByName(t, contextValue.Messages, "screenshot")
	require.NotEmpty(t, content)
	assert.Equal(t, ai.Text("captured"), content[0])
	assert.Equal(t, ai.Text(mediaRefusalText(mediaKindImage, "screenshot")), content[1])

	// The provider never receives the image: the second request answers the call
	// with the refusal text alone.
	requests := model.Requests()
	require.Len(t, requests, 2)

	for _, part := range toolResultParts(requests[1].Messages) {
		for _, media := range part.Content {
			if _, ok := media.(ai.ImagePart); ok {
				t.Fatalf("provider request carried an image the model cannot accept: %#v", media)
			}
		}
	}
}

// TestRuntimeRefusesPendingToolMediaTheModelCannotAccept drives the gate through
// the paused path: the extension tool pauses on its first call, so the result
// is committed by the pending runner rather than the interaction's AfterTool
// chain, and the refusal must still be what reaches the transcript.
func TestRuntimeRefusesPendingToolMediaTheModelCannotAccept(t *testing.T) {
	t.Parallel()

	var gateCalls int

	imageTool := agent.NewToolParts("screenshot", "Capture the screen.", func(
		context.Context,
		struct{},
	) ([]ai.Part, error) {
		return []ai.Part{
			ai.Text("captured"),
			ai.ImageData("image/png", []byte{1, 2, 3}),
		}, nil
	})

	ext, err := extension.NewDefinition(extension.Descriptor{
		ID: "paused-screenshot-tool", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{
			Tools: []extension.Tool{{Value: imageTool, Risk: catalog.RiskRead}},
			Hooks: extension.Hooks{BeforeTool: func(
				_ context.Context,
				info agent.ToolCallInfo,
			) agent.ToolDecision {
				if info.Name == "screenshot" {
					gateCalls++

					if gateCalls == 1 {
						return agent.ToolDecision{Action: agent.ToolDecisionPause}
					}
				}

				return agent.ToolDecision{}
			}},
		}, nil
	})
	require.NoError(t, err)

	model := newRuntimeModel(
		runtimeToolResponse("call-shot", "screenshot", `{}`),
		runtimeTextResponse("handled"),
	)
	runtime := openTestRuntimeWithExtensions(t, model, ext)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("screenshot")))

	contextValue, err := runtime.session.Context()
	require.NoError(t, err)

	content := toolResultContentByName(t, contextValue.Messages, "screenshot")
	require.NotEmpty(t, content)
	assert.Equal(t, ai.Text("captured"), content[0])
	assert.Equal(t, ai.Text(mediaRefusalText(mediaKindImage, "screenshot")), content[1])

	for _, part := range toolResultParts(contextValue.Messages) {
		for _, media := range part.Content {
			if _, ok := media.(ai.ImagePart); ok {
				t.Fatalf("a paused tool result carried an image the model cannot accept: %#v", media)
			}
		}
	}
}

// TestRuntimePendingResultHookGatesMedia covers the approval-controller path: an
// approval-paused call is executed by the approval controller rather than the
// interaction's AfterTool chain, so the registered pending result hook is the
// only place the gate can refuse its media.
func TestRuntimePendingResultHookGatesMedia(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("ok")))
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	parts, isError := runtime.hookAfterPendingTool(t.Context(), agent.ToolCall{
		ID: "call-shot", Name: "screenshot",
	}, []ai.Part{
		ai.Text("captured"),
		ai.ImageData("image/png", []byte{1, 2, 3}),
	}, false)

	require.False(t, isError, "a refusal is feedback, not a failed call")
	require.Len(t, parts, 2)
	assert.Equal(t, ai.Text(mediaRefusalText(mediaKindImage, "screenshot")), parts[1])
}

// TestChildControlScopePendingResultHookGatesMedia covers the same path for a
// child: its paused call is resolved by the child's pending result hook.
func TestChildControlScopePendingResultHookGatesMedia(t *testing.T) {
	t.Parallel()

	blind := &visionRuntimeModel{newRuntimeModel(), ai.Capabilities{Text: true, Tools: true}}
	scope := &childControlScope{factory: childScopeFactory{model: blind}}

	parts, isError := scope.afterPendingTool(t.Context(), agent.ToolCall{
		ID: "call-shot", Name: "screenshot",
	}, []ai.Part{
		ai.Text("captured"),
		ai.ImageData("image/png", []byte{1, 2, 3}),
	}, false)

	require.False(t, isError)
	require.Len(t, parts, 2)
	assert.Equal(t, ai.Text(mediaRefusalText(mediaKindImage, "screenshot")), parts[1])
}

func toolResultContentByName(t *testing.T, messages ai.Messages, name string) []ai.Part {
	t.Helper()

	for _, message := range messages {
		tool, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}

		for _, result := range tool.Parts {
			if result.Name == name {
				return result.Content
			}
		}
	}

	require.FailNow(t, "no tool result for "+name)

	return nil
}

func toolResultParts(messages ai.Messages) []ai.ToolResultPart {
	var out []ai.ToolResultPart

	for _, message := range messages {
		tool, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}

		out = append(out, tool.Parts...)
	}

	return out
}
