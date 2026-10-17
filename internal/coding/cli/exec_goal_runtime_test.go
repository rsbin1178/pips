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
			model := &cliGoalModel{structuredOutput: true}
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
			assert.Equal(t, cliGoalPanelSize, model.verifications, "one default three-skeptic panel")
			assert.ElementsMatch(t, []string{"read", "ls", "glob", "grep"}, model.verifierTools)

			// A declared model keeps the provider's own schema mechanism on both
			// role calls.
			assert.Equal(t, []bool{true, true}, model.assessmentFormats)
			assert.Equal(t, cliGoalPanelBools(true), model.verdictFormats)

			// No role call carried an output cap: the Goal's budget bounds the
			// run, not one call, so the provider's own default applies.
			assert.Equal(t, []*int{nil, nil}, model.assessmentCaps)
			assert.Equal(t, cliGoalPanelReadCaps(), model.readCaps)
			assert.Equal(t, cliGoalPanelCaps(), model.verdictCaps)

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

// TestExecGoalCompletesOnAPortableSchemaModel pins the fallback transport
// end to end through the real Runtime: pips exec --goal completes on a model
// that declares no structured output, because both the assessment and the audit
// answer through the prompt-only JSON convention.
func TestExecGoalCompletesOnAPortableSchemaModel(t *testing.T) {
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
	presenter := newExecPresenter(outputPlain, stdout, stderr, false)
	request := coding.GoalRequest{Condition: "proof.txt contains shipped proof", MaxTokens: 10000}
	require.NoError(t, runExecRequest(t.Context(), runtime, presenter, request.Condition, &request))

	assert.True(t, runtime.Snapshot().Goal.Completed())
	assert.Equal(t, 2, model.evaluations)
	assert.Equal(t, cliGoalPanelSize, model.verifications, "one default three-skeptic panel")
	assert.Equal(t, "The recorded file contains shipped proof.\n", stdout.String())
	assert.Contains(t, stderr.String(), "goal completed")

	// Neither role call used the provider's own schema mechanism.
	assert.Equal(t, []bool{false, false}, model.assessmentFormats)
	assert.Equal(t, cliGoalPanelBools(false), model.verdictFormats)

	// No role call carried an output cap either, on the read rounds or the
	// verdict.
	assert.Equal(t, []*int{nil, nil}, model.assessmentCaps)
	assert.Equal(t, cliGoalPanelReadCaps(), model.readCaps)
	assert.Equal(t, cliGoalPanelCaps(), model.verdictCaps)
}

// cliGoalPanelSize is the default verifier panel's size, which the runtime builds
// for every Goal.
const cliGoalPanelSize = 3

// cliGoalPanelBools returns one entry per skeptic of the default panel.
func cliGoalPanelBools(value bool) []bool {
	values := make([]bool, 0, cliGoalPanelSize)
	for range cliGoalPanelSize {
		values = append(values, value)
	}

	return values
}

// cliGoalPanelCaps returns the default panel's per-call output caps.
func cliGoalPanelCaps() []*int {
	caps := make([]*int, 0, cliGoalPanelSize)
	for range cliGoalPanelSize {
		caps = append(caps, nil)
	}

	return caps
}

// cliGoalPanelReadCaps returns the default panel's read-round caps: each of the
// three skeptics makes one tool-carrying round before it stops reading.
func cliGoalPanelReadCaps() []*int {
	caps := make([]*int, 0, cliGoalPanelSize*2)
	for range cliGoalPanelSize * 2 {
		caps = append(caps, nil)
	}

	return caps
}

type cliGoalModel struct {
	mu                sync.Mutex
	structuredOutput  bool
	workCalls         int
	evaluations       int
	verificationReads int
	verifications     int
	verifierTools     []string
	assessmentFormats []bool
	verdictFormats    []bool
	assessmentCaps    []*int
	readCaps          []*int
	verdictCaps       []*int
}

// Generate answers the Goal's assessment and audit calls. Each role is
// identified by its system text, never by ResponseFormat, because the portable
// transport omits it on both.
func (m *cliGoalModel) Generate(_ context.Context, request ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	system := cliGoalSystemText(request)

	switch {
	case strings.Contains(system, "completion evaluator"):
		m.evaluations++
		m.assessmentFormats = append(m.assessmentFormats, request.ResponseFormat != nil)
		m.assessmentCaps = append(m.assessmentCaps, cloneCLITokenCap(request.MaxTokens))

		// Phase AB's assessment document: the reference's four fields.
		decision := "continue"
		if m.evaluations > 1 {
			decision = "candidate_complete"
		}

		return cliGoalResponse(ai.AssistantText(fmt.Sprintf(
			`{"decision":%q,"evidence":"Inspect the actual recorded file evidence.",`+
				`"next_step":"inspect the recorded file evidence","blocker_key":""}`,
			decision,
		))), nil
	case strings.Contains(system, "independent Coding Goal evidence auditor"):
		return m.audit(request)
	default:
		return nil, errors.New("unexpected structured request in CLI Goal fixture")
	}
}

// audit answers the auditor's read rounds and then its final verdict. A read
// round carries the read-only catalog; the verdict call has no tools at all,
// which is what distinguishes them when ResponseFormat is absent.
func (m *cliGoalModel) audit(request ai.Request) (*ai.Response, error) {
	if len(request.Tools) > 0 {
		m.verificationReads++
		m.readCaps = append(m.readCaps, cloneCLITokenCap(request.MaxTokens))

		if m.verifierTools == nil {
			for _, tool := range request.Tools {
				m.verifierTools = append(m.verifierTools, tool.Name)
			}
		}

		for _, message := range request.Messages {
			if _, ok := message.(ai.ToolMessage); ok {
				return cliGoalResponse(ai.AssistantText("Ready to evaluate the file read.")), nil
			}
		}

		return cliGoalResponse(ai.Assistant(ai.ToolCallPart{ID: "verify-file", Name: "read", Args: ai.JSON(`{"path":"proof.txt"}`)})), nil
	}

	evidence := cliGoalEvidence(request.Messages)
	if len(evidence.Records) == 0 {
		return nil, errors.New("CLI verifier fixture received no actual evidence")
	}

	m.verifications++
	m.verdictFormats = append(m.verdictFormats, request.ResponseFormat != nil)
	m.verdictCaps = append(m.verdictCaps, cloneCLITokenCap(request.MaxTokens))

	encoded, err := json.Marshal(goalflow.SkepticVerdict{
		Refuted: false, Evidence: "The recorded result and independent read agree.",
		Confidence: goalflow.ConfidenceHigh, Blocking: goalflow.BlockingNone,
	})
	if err != nil {
		return nil, err
	}

	return cliGoalResponse(ai.AssistantText(string(encoded))), nil
}

// cloneCLITokenCap copies a request's output cap so the record cannot alias a
// caller-owned pointer.
func cloneCLITokenCap(maxTokens *int) *int {
	if maxTokens == nil {
		return nil
	}

	return ai.Ptr(*maxTokens)
}

func cliGoalSystemText(request ai.Request) string {
	system, _, err := request.Messages.SplitSystem()
	if err != nil {
		return ""
	}

	return ai.JoinSystemText(system)
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
func (m *cliGoalModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: m.structuredOutput}
}
