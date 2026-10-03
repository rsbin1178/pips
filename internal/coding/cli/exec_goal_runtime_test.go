package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecGoalUsesRealRuntimeAndReadOnlyVerification(t *testing.T) {
	t.Parallel()

	for _, mode := range []outputMode{outputPlain, outputJSONL} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			require.NoError(t, os.WriteFile(root+"/proof.txt", []byte("shipped proof\n"), 0o600))
			ws, err := workspace.Open(root)
			require.NoError(t, err)
			layout, err := paths.New(t.TempDir() + "/home")
			require.NoError(t, err)

			cfg := config.Defaults()
			cfg.Model = config.ModelRef{Provider: ai.ProviderOpenAI, Model: "cli-goal"}
			model := new(cliGoalModel)
			runtime, err := coding.Open(t.Context(), coding.OpenOptions{
				Workspace: ws, Config: cfg, Paths: layout, Model: model,
				Execution: coding.ExecutionOptions{SandboxProbe: func(context.Context, *execution.Executor) error { return nil }},
			})
			require.NoError(t, err)

			stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
			presenter := newExecPresenter(mode, stdout, stderr, false)
			request := coding.GoalRequest{Condition: "proof.txt contains shipped proof", MaxTokens: 10000}
			require.NoError(t, runExecRequest(t.Context(), runtime, presenter, request.Condition, &request))
			assert.True(t, runtime.Snapshot().Goal.Completed())
			assert.Equal(t, 2, model.evaluations)
			assert.Equal(t, 4, model.workCalls, "two real read-and-answer work segments")
			assert.Equal(t, 1, model.verifications)
			assert.ElementsMatch(t, []string{"read", "ls", "glob", "grep"}, model.verifierTools)

			if mode == outputPlain {
				assert.Equal(t, "The recorded file contains shipped proof.\n", stdout.String())
				assert.Contains(t, stderr.String(), "goal completed")

				return
			}

			var goalEvents int

			for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
				event, err := coding.UnmarshalEvent([]byte(line))
				require.NoError(t, err)

				if event.Type == coding.EventGoalChanged {
					goalEvents++
				}
			}

			assert.Positive(t, goalEvents)
			assert.NotContains(t, stdout.String(), request.Condition)
		})
	}
}

type cliGoalModel struct {
	mu                sync.Mutex
	workCalls         int
	evaluations       int
	verificationReads int
	verifications     int
	verifierTools     []string
}

func (m *cliGoalModel) Generate(_ context.Context, request ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if request.ResponseFormat != nil {
		switch request.ResponseFormat.Name {
		case "goal_evaluation":
			m.evaluations++

			outcome := "continue"
			if m.evaluations > 1 {
				outcome = "complete"
			}

			return cliGoalResponse(ai.AssistantText(fmt.Sprintf(`{"outcome":%q,"reason":"Inspect the actual recorded file evidence."}`, outcome))), nil
		case "coding_goal_verification":
			evidence := cliGoalEvidence(request.Messages)
			if len(evidence.Records) == 0 {
				return nil, errors.New("CLI verifier fixture received no actual evidence")
			}

			m.verifications++

			encoded, err := json.Marshal(goalflow.Verdict{
				Verified: true, Reason: "The recorded result and independent read agree.", Gaps: []string{},
				References: []string{evidence.Records[0].ID, "read:verify-file"},
			})
			if err != nil {
				return nil, err
			}

			return cliGoalResponse(ai.AssistantText(string(encoded))), nil
		default:
			return nil, errors.New("unexpected structured request in CLI Goal fixture")
		}
	}

	m.verificationReads++
	if m.verificationReads == 1 {
		for _, tool := range request.Tools {
			m.verifierTools = append(m.verifierTools, tool.Name)
		}

		return cliGoalResponse(ai.Assistant(ai.ToolCallPart{ID: "verify-file", Name: "read", Args: ai.JSON(`{"path":"proof.txt"}`)})), nil
	}

	return cliGoalResponse(ai.AssistantText("Ready to evaluate the file read.")), nil
}

func (m *cliGoalModel) Stream(_ context.Context, _ ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		m.mu.Lock()
		m.workCalls++
		call := m.workCalls
		m.mu.Unlock()

		events := []ai.StreamEvent{{Type: ai.StreamMessageStart, Provider: ai.ProviderOpenAI, Model: "cli-goal"}}
		finish := ai.FinishStop

		if call%2 == 1 {
			events = append(events,
				ai.StreamEvent{Type: ai.StreamToolCallStart, ToolCallID: fmt.Sprintf("worker-read-%d", call), ToolCallName: "read"},
				ai.StreamEvent{Type: ai.StreamToolCallDelta, ArgsDelta: `{"path":"proof.txt"}`},
				ai.StreamEvent{Type: ai.StreamToolCallEnd})
			finish = ai.FinishToolCalls
		} else {
			events = append(events, ai.StreamEvent{Type: ai.StreamTextDelta, Text: "The recorded file contains shipped proof."})
		}

		events = append(events, ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: finish, Usage: &ai.Usage{InputTokens: 10, OutputTokens: 5}})
		for _, event := range events {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func cliGoalEvidence(messages ai.Messages) goalflow.Evidence {
	for _, message := range messages {
		user, ok := message.(ai.UserMessage)
		if !ok {
			continue
		}

		for _, part := range user.Parts {
			text, ok := part.(ai.TextPart)
			if !ok {
				continue
			}

			var payload struct {
				Evidence goalflow.Evidence `json:"evidence"`
			}
			if json.Unmarshal([]byte(text.Text), &payload) == nil && len(payload.Evidence.Records) > 0 {
				return payload.Evidence
			}
		}
	}

	return goalflow.Evidence{}
}

func cliGoalResponse(message ai.AssistantMessage) *ai.Response {
	return &ai.Response{
		Provider: ai.ProviderOpenAI, Model: "cli-goal", Message: message,
		FinishReason: ai.FinishStop, Usage: ai.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

func (*cliGoalModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*cliGoalModel) ModelID() string       { return "cli-goal" }
func (*cliGoalModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}
