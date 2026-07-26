package extension_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/ai"
)

func TestComposeHooksUsesDocumentedOrdering(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	var events []string

	isError := true
	hooks := extension.ComposeHooks(
		extension.Hooks{
			Observe: func(context.Context, agent.Event) {
				events = append(events, "observe-1")
			},
			BeforeTool: func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
				events = append(events, "gate-1")
				return agent.ToolDecision{}
			},
			AfterTool: func(_ context.Context, info agent.ToolResultInfo) *agent.ToolResultOverride {
				events = append(events, "after-1:"+resultText(info.Result))
				return &agent.ToolResultOverride{Content: []ai.Part{ai.Text("first")}}
			},
		},
		extension.Hooks{
			Observe: func(context.Context, agent.Event) {
				events = append(events, "observe-2")
			},
			BeforeTool: func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
				events = append(events, "gate-2")
				return agent.DenyTool("denied")
			},
			AfterTool: func(_ context.Context, info agent.ToolResultInfo) *agent.ToolResultOverride {
				events = append(events, "after-2:"+resultText(info.Result))
				return &agent.ToolResultOverride{IsError: &isError}
			},
		},
		extension.Hooks{
			BeforeTool: func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
				events = append(events, "gate-3")
				return agent.ToolDecision{}
			},
		},
	)

	hooks.Observe(ctx, agent.Event{Type: agent.EventRunStart})

	decision := hooks.BeforeTool(ctx, agent.ToolCallInfo{})
	if decision.Action != agent.ToolDecisionDeny || decision.Reason != "denied" {
		t.Fatalf("gate decision = %#v", decision)
	}

	override := hooks.AfterTool(ctx, agent.ToolResultInfo{
		Result: ai.ToolResultPart{Content: []ai.Part{ai.Text("raw")}},
	})
	if override == nil || resultText(ai.ToolResultPart{Content: override.Content}) != "first" {
		t.Fatalf("combined content override = %#v", override)
	}

	if override.IsError == nil || !*override.IsError {
		t.Fatalf("combined error override = %#v", override)
	}

	want := []string{
		"observe-1", "observe-2",
		"gate-1", "gate-2",
		"after-1:raw", "after-2:first",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("hook events = %v, want %v", events, want)
	}
}

func TestComposeHooksPipelinesContextWithDefensiveCopies(t *testing.T) {
	t.Parallel()

	input := []ai.Message{ai.User(ai.ImageData("image/png", []byte("raw")))}
	hooks := extension.ComposeHooks(
		extension.Hooks{TransformContext: func(_ context.Context, messages []ai.Message) ([]ai.Message, error) {
			image, ok := messages[0].Parts[0].(ai.ImagePart)
			if !ok {
				t.Fatalf("first part type = %T, want ai.ImagePart", messages[0].Parts[0])
			}

			image.Source.Data[0] = 'x'
			messages[0].Parts[0] = image

			return append(messages, ai.AssistantText("first")), nil
		}},
		extension.Hooks{TransformContext: func(_ context.Context, messages []ai.Message) ([]ai.Message, error) {
			if len(messages) != 2 {
				t.Fatalf("second transform received %d messages", len(messages))
			}

			return append(messages, ai.AssistantText("second")), nil
		}},
	)

	got, err := hooks.TransformContext(context.Background(), input)
	if err != nil {
		t.Fatalf("transform context: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("transformed message count = %d, want 3", len(got))
	}

	original, ok := input[0].Parts[0].(ai.ImagePart)
	if !ok {
		t.Fatalf("original part type = %T, want ai.ImagePart", input[0].Parts[0])
	}

	if got := string(original.Source.Data); got != "raw" {
		t.Fatalf("input data was mutated: %q", got)
	}

	transformed, ok := got[0].Parts[0].(ai.ImagePart)
	if !ok {
		t.Fatalf("transformed part type = %T, want ai.ImagePart", got[0].Parts[0])
	}

	if got := string(transformed.Source.Data); got != "xaw" {
		t.Fatalf("transformed data = %q", got)
	}
}

func TestComposeHooksStopsContextPipelineOnError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("transform failed")
	called := false
	hooks := extension.ComposeHooks(
		extension.Hooks{TransformContext: func(context.Context, []ai.Message) ([]ai.Message, error) {
			return nil, wantErr
		}},
		extension.Hooks{TransformContext: func(context.Context, []ai.Message) ([]ai.Message, error) {
			called = true
			return nil, nil
		}},
	)

	_, err := hooks.TransformContext(context.Background(), nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("transform error = %v", err)
	}

	if called {
		t.Fatal("transform after failure was called")
	}
}

func TestComposeHooksMergesPrepareTurnUpdates(t *testing.T) {
	t.Parallel()

	firstTool := agent.NewTool("first", "first", func(context.Context, struct{}) (string, error) {
		return "first", nil
	})
	secondTool := agent.NewTool("second", "second", func(context.Context, struct{}) (string, error) {
		return "second", nil
	})

	var order []string

	hooks := extension.ComposeHooks(
		extension.Hooks{PrepareTurn: func(context.Context, agent.RunInfo) agent.TurnUpdate {
			order = append(order, "first")

			return agent.TurnUpdate{
				ReplaceMessages: []ai.Message{ai.UserText("compacted")},
				Tools:           []agent.Tool{firstTool},
			}
		}},
		extension.Hooks{PrepareTurn: func(context.Context, agent.RunInfo) agent.TurnUpdate {
			order = append(order, "second")

			return agent.TurnUpdate{Tools: []agent.Tool{secondTool}}
		}},
	)

	update := hooks.PrepareTurn(context.Background(), agent.RunInfo{})

	if want := []string{"first", "second"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("prepare order = %v, want %v", order, want)
	}

	if len(update.ReplaceMessages) != 1 {
		t.Fatalf("replace messages = %#v", update.ReplaceMessages)
	}

	text, ok := update.ReplaceMessages[0].Parts[0].(ai.TextPart)
	if !ok || text.Text != "compacted" {
		t.Fatalf("replace messages = %#v", update.ReplaceMessages)
	}

	if len(update.Tools) != 1 || update.Tools[0].Decl().Name != "second" {
		t.Fatalf("tools = %#v", update.Tools)
	}
}

func TestComposeHooksStopsPrepareTurnPipelineOnError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("prepare failed")
	called := false
	hooks := extension.ComposeHooks(
		extension.Hooks{PrepareTurn: func(context.Context, agent.RunInfo) agent.TurnUpdate {
			return agent.TurnUpdate{Err: wantErr}
		}},
		extension.Hooks{PrepareTurn: func(context.Context, agent.RunInfo) agent.TurnUpdate {
			called = true

			return agent.TurnUpdate{}
		}},
	)

	update := hooks.PrepareTurn(context.Background(), agent.RunInfo{})
	if !errors.Is(update.Err, wantErr) {
		t.Fatalf("prepare error = %v, want %v", update.Err, wantErr)
	}

	if called {
		t.Fatal("prepare hook after failure was called")
	}
}

func resultText(result ai.ToolResultPart) string {
	if len(result.Content) == 0 {
		return ""
	}

	text, _ := result.Content[0].(ai.TextPart)

	return text.Text
}
