//nolint:wsl_v5 // Each transport fixture keeps its scripted model and assertions adjacent.
package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goalTransportModel scripts a Goal run on either structured-output transport.
// It identifies each role call by its system text and its tool list, never by
// ResponseFormat, because the portable transport omits it.
type goalTransportModel struct {
	*runtimeModel

	mu               sync.Mutex
	structuredOutput bool

	assessments []string
	verdicts    []string

	assessmentFormats []bool
	verdictFormats    []bool
	assessmentSystems []string
	verdictSystems    []string
	assessmentCaps    []*int
	readCaps          []*int
	verdictCaps       []*int
}

func newGoalTransportModel(responses ...*ai.Response) *goalTransportModel {
	return &goalTransportModel{runtimeModel: newRuntimeModel(responses...)}
}

func (m *goalTransportModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: m.structuredOutput}
}

func (m *goalTransportModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	system := requestSystemText(request)

	switch {
	case strings.Contains(system, "completion evaluator"):
		return m.assess(request, system)
	case strings.Contains(system, "independent Coding Goal evidence auditor"):
		return m.audit(request, system)
	default:
		return m.runtimeModel.Generate(ctx, request)
	}
}

func (m *goalTransportModel) assess(request ai.Request, system string) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.assessmentFormats = append(m.assessmentFormats, request.ResponseFormat != nil)
	m.assessmentSystems = append(m.assessmentSystems, system)
	m.assessmentCaps = append(m.assessmentCaps, cloneTokenCap(request.MaxTokens))

	answer := `{"decision":"continue","evidence":"the evidence does not prove the condition",` +
		`"next_step":"collect the missing evidence","blocker_key":""}`
	if len(m.assessments) > 0 {
		answer, m.assessments = m.assessments[0], m.assessments[1:]
	}

	return runtimeTextResponse(answer), nil
}

func (m *goalTransportModel) audit(request ai.Request, system string) (*ai.Response, error) {
	// A read round carries the bounded read-only catalog; the verdict call has no
	// tools at all, on either transport.
	if len(request.Tools) > 0 {
		m.mu.Lock()
		m.readCaps = append(m.readCaps, cloneTokenCap(request.MaxTokens))
		m.mu.Unlock()

		for _, message := range request.Messages {
			if _, ok := message.(ai.ToolMessage); ok {
				return runtimeTextResponse("evidence read"), nil
			}
		}

		return runtimeToolResponse("audit-read", "read", `{"path":"proof.txt"}`), nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.verdictFormats = append(m.verdictFormats, request.ResponseFormat != nil)
	m.verdictSystems = append(m.verdictSystems, system)
	m.verdictCaps = append(m.verdictCaps, cloneTokenCap(request.MaxTokens))

	if len(m.verdicts) > 0 {
		answer := m.verdicts[0]
		m.verdicts = m.verdicts[1:]

		return runtimeTextResponse(answer), nil
	}

	return runtimeTextResponse(verifiedGoalTransportVerdict(request.Messages)), nil
}

func (m *goalTransportModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

// callFormats reports, per role, whether the call carried a ResponseFormat
// alongside the system text it saw.
func (m *goalTransportModel) callFormats() ([]bool, []bool, []string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Clone(m.assessmentFormats), slices.Clone(m.verdictFormats),
		slices.Clone(m.assessmentSystems), slices.Clone(m.verdictSystems)
}

// callCaps reports, per role, the output cap each call carried, nil when it
// carried none.
func (m *goalTransportModel) callCaps() ([]*int, []*int, []*int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Clone(m.assessmentCaps), slices.Clone(m.readCaps), slices.Clone(m.verdictCaps)
}

// cloneTokenCap copies a request's output cap so the record cannot alias a
// caller-owned pointer.
func cloneTokenCap(maxTokens *int) *int {
	if maxTokens == nil {
		return nil
	}

	return ai.Ptr(*maxTokens)
}

// verifiedGoalTransportVerdict is the default audit answer: it cites the recorded
// evidence and the auditor's own read, so the proof gate passes.
func verifiedGoalTransportVerdict(messages ai.Messages) string {
	references := []string{"read:audit-read"}
	for _, message := range messages {
		var payload struct {
			Evidence goalflow.Evidence `json:"evidence"`
		}
		if json.Unmarshal([]byte(runtimeMessageText(message)), &payload) == nil {
			for _, record := range payload.Evidence.Records {
				references = append(references, record.ID)
			}
		}
	}

	data, _ := json.Marshal(goalflow.Verdict{
		Verified: true, Reason: "the recorded read matches the actual file", Gaps: []string{}, References: references,
	})

	return string(data)
}

// goalGapObserved reports whether a Goal projection carried a gap containing
// substring while the Goal was not completed.
func goalGapObserved(events []Event, substring string) bool {
	for _, event := range events {
		changed, ok := event.Payload.(GoalChanged)
		if !ok || changed.State.Completed() {
			continue
		}

		for _, gap := range changed.State.Gaps {
			if strings.Contains(gap, substring) {
				return true
			}
		}
	}

	return false
}

// scriptGoalTransportSegments appends count read-and-answer work segments, the
// smallest real segment that records tool evidence and then answers.
func scriptGoalTransportSegments(model *goalTransportModel, count int) {
	for index := range count {
		model.responses = append(model.responses,
			runtimeToolResponse(fmt.Sprintf("proof-read-%d", index), "read", `{"path":"proof.txt"}`),
			runtimeTextResponse("candidate complete"),
		)
	}
}

func writeGoalTransportProof(t *testing.T, runtime *Runtime) {
	t.Helper()

	require.NoError(t, os.WriteFile(
		filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600,
	))
}

// TestRuntimeGoalCompletesOnAPortableSchemaModel pins the fallback transport end
// to end: a Goal starts, works, assesses, audits and completes on a model that
// declares no structured output, with the schema convention in the system
// message and no ResponseFormat on either role call.
func TestRuntimeGoalCompletesOnAPortableSchemaModel(t *testing.T) {
	t.Parallel()

	model := newGoalTransportModel()
	scriptGoalTransportSegments(model, 1)
	model.assessments = []string{`{"decision":"candidate_complete","evidence":"the recorded read proves the condition",` +
		`"next_step":"verify the candidate","blocker_key":""}`}
	runtime := openTestRuntime(t, model)
	writeGoalTransportProof(t, runtime)

	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 1, state.Attempts)
	assertEventSequence(t, events)

	assessmentFormats, verdictFormats, assessmentSystems, verdictSystems := model.callFormats()
	require.Equal(t, []bool{false}, assessmentFormats)
	require.Equal(t, []bool{false}, verdictFormats)
	assert.Contains(t, assessmentSystems[0], "Native structured output is unavailable.")
	assert.Contains(t, assessmentSystems[0], `"decision"`)
	assert.Contains(t, verdictSystems[0], "Native structured output is unavailable.")
	assert.Contains(t, verdictSystems[0], `"verified"`)

	// No role call carried an output cap: the Goal's budget bounds the run, not
	// one call, so the provider's own default applies. The auditor's read rounds
	// are pinned separately because they carry tools and the verdict does not.
	assessmentCaps, readCaps, verdictCaps := model.callCaps()
	require.Equal(t, []*int{nil}, assessmentCaps)
	require.Equal(t, []*int{nil, nil}, readCaps)
	require.Equal(t, []*int{nil}, verdictCaps)
}

// TestRuntimeGoalSendsTheNativeSchemaOnADeclaredModel is the no-regression half:
// a model that declares structured output still gets the provider's own schema
// mechanism on both role calls.
func TestRuntimeGoalSendsTheNativeSchemaOnADeclaredModel(t *testing.T) {
	t.Parallel()

	model := newGoalTransportModel()
	scriptGoalTransportSegments(model, 1)
	model.structuredOutput = true
	model.assessments = []string{`{"decision":"candidate_complete","evidence":"the recorded read proves the condition",` +
		`"next_step":"verify the candidate","blocker_key":""}`}
	runtime := openTestRuntime(t, model)
	writeGoalTransportProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)
	require.True(t, runtime.GoalSnapshot().Completed(), "%+v", runtime.GoalSnapshot())

	assessmentFormats, verdictFormats, assessmentSystems, _ := model.callFormats()
	require.Equal(t, []bool{true}, assessmentFormats)
	require.Equal(t, []bool{true}, verdictFormats)
	assert.NotContains(t, assessmentSystems[0], "Native structured output is unavailable.")

	// The declared transport carries no cap either, on the read rounds or the
	// verdict.
	assessmentCaps, readCaps, verdictCaps := model.callCaps()
	require.Equal(t, []*int{nil}, assessmentCaps)
	require.Equal(t, []*int{nil, nil}, readCaps)
	require.Equal(t, []*int{nil}, verdictCaps)
}

// TestRuntimeGoalCarriesAnUnverifiedPortableVerdictsGapsForward pins the "not
// verified → continue with the gaps" mapping over the portable transport.
func TestRuntimeGoalCarriesAnUnverifiedPortableVerdictsGapsForward(t *testing.T) {
	t.Parallel()

	const gap = "run the actual migration check"

	model := newGoalTransportModel()
	scriptGoalTransportSegments(model, 2)
	model.assessments = []string{
		`{"decision":"candidate_complete","evidence":"the first candidate looks done",` +
			`"next_step":"verify the candidate","blocker_key":""}`,
		`{"decision":"candidate_complete","evidence":"the second candidate looks done",` +
			`"next_step":"verify the candidate","blocker_key":""}`,
	}
	model.verdicts = []string{
		`{"verified":false,"reason":"the recorded read does not cover the migration","gaps":["` + gap + `"],"references":[]}`,
	}
	runtime := openTestRuntime(t, model)
	writeGoalTransportProof(t, runtime)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 2, state.Attempts)

	// The auditor's gaps are the Goal state's gaps, and the next work segment
	// receives them as the loop's own feedback.
	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.True(t, requestContainsText(requests[2], "Independent evidence gaps"))
	assert.True(t, requestContainsText(requests[2], gap))

	_, verdictFormats, _, _ := model.callFormats()
	assert.Equal(t, []bool{false, false}, verdictFormats)
}

// TestRuntimeGoalTreatsAMalformedPortableVerdictAsNotAchieved pins the wrong-shape
// rule at the audit boundary: a verdict that is not one JSON object is "not
// achieved", and its reason reaches the next work segment.
func TestRuntimeGoalTreatsAMalformedPortableVerdictAsNotAchieved(t *testing.T) {
	t.Parallel()

	model := newGoalTransportModel()
	scriptGoalTransportSegments(model, 2)
	model.assessments = []string{
		`{"decision":"candidate_complete","evidence":"the first candidate looks done",` +
			`"next_step":"verify the candidate","blocker_key":""}`,
		`{"decision":"candidate_complete","evidence":"the second candidate looks done",` +
			`"next_step":"verify the candidate","blocker_key":""}`,
	}
	model.verdicts = []string{"not the required verdict object"}
	runtime := openTestRuntime(t, model)
	writeGoalTransportProof(t, runtime)

	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	// The malformed verdict was not a completion: the Goal projection that carried
	// its reason stayed open.
	assert.True(t, goalGapObserved(events, "not the required JSON verdict object"))

	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.True(t, requestContainsText(requests[2], "Independent evidence gaps"))
	assert.True(t, requestContainsText(requests[2], "not the required JSON verdict object"))

	// The run then ended deterministically on the second, verified verdict.
	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 2, state.Attempts)

	_, verdictFormats, _, _ := model.callFormats()
	assert.Equal(t, []bool{false, false}, verdictFormats)
}

// TestRuntimeGoalTreatsAContractBreakingPortableVerdictAsNotAchieved pins the
// auditor's rule at the audit boundary: a verdict that parses but breaks its own
// contract is "not achieved", and its reason reaches the next work segment
// exactly like a verdict that did not parse at all.
func TestRuntimeGoalTreatsAContractBreakingPortableVerdictAsNotAchieved(t *testing.T) {
	t.Parallel()

	overBounds := make([]string, goalflow.MaxGaps+1)
	for index := range overBounds {
		overBounds[index] = fmt.Sprintf("gap-%d", index)
	}

	overBoundsVerdict, err := json.Marshal(goalflow.Verdict{
		Reason: "the recorded read does not cover the migration", Gaps: overBounds, References: []string{},
	})
	require.NoError(t, err)

	tests := []struct {
		name   string
		answer string
		gap    string
	}{
		{
			name:   "empty reason",
			answer: `{"verified":false,"reason":"","gaps":["the recorded read is stale"],"references":[]}`,
			gap:    "carries no reason",
		},
		{
			name:   "over-bounds gaps",
			answer: string(overBoundsVerdict),
			gap:    fmt.Sprintf("carries more than %d gaps", goalflow.MaxGaps),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := newGoalTransportModel()
			scriptGoalTransportSegments(model, 2)
			model.assessments = []string{
				`{"decision":"candidate_complete","evidence":"the first candidate looks done",` +
					`"next_step":"verify the candidate","blocker_key":""}`,
				`{"decision":"candidate_complete","evidence":"the second candidate looks done",` +
					`"next_step":"verify the candidate","blocker_key":""}`,
			}
			model.verdicts = []string{test.answer}
			runtime := openTestRuntime(t, model)
			writeGoalTransportProof(t, runtime)

			events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
			require.NoError(t, err)

			// The unusable verdict was not a completion: the Goal projection that
			// carried its reason stayed open.
			assert.True(t, goalGapObserved(events, test.gap))

			requests := model.Requests()
			require.Len(t, requests, 4)
			assert.True(t, requestContainsText(requests[2], "Independent evidence gaps"))
			assert.True(t, requestContainsText(requests[2], test.gap))

			// The run then ended deterministically on the second, verified verdict.
			state := runtime.GoalSnapshot()
			require.True(t, state.Completed(), "%+v", state)
			assert.Equal(t, 2, state.Attempts)
		})
	}
}

// TestRuntimeGoalStopsOnAnUnusablePortableAssessment pins the assessment's own
// rule at the runtime boundary: an unusable answer is retried once and then
// stops the run, because the harness cannot judge. The Goal is neither completed
// nor continued on a gap-only assessment.
func TestRuntimeGoalStopsOnAnUnusablePortableAssessment(t *testing.T) {
	t.Parallel()

	model := newGoalTransportModel()
	scriptGoalTransportSegments(model, 1)
	model.assessments = []string{"not the required assessment object", "not the required assessment object"}
	runtime := openTestRuntime(t, model)
	writeGoalTransportProof(t, runtime)

	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bounded retry")
	assert.Contains(t, err.Error(), "stopped rather than treated as complete")
	assert.Contains(t, err.Error(), "use /goal resume to retry")

	// The old gap-only continuation is gone: no Goal projection carried the
	// answer as a gap, and the Goal stopped instead of continuing.
	assert.False(t, goalGapObserved(events, "not the required JSON object"))

	state := runtime.GoalSnapshot()
	assert.False(t, state.Completed())
	assert.Equal(t, GoalInterrupted, state.Status)

	assessmentFormats, _, _, _ := model.callFormats()
	assert.Len(t, assessmentFormats, 2, "the unusable assessment was retried once")
}

// TestRuntimeUpdateGoalCompletionIsAuditedOverThePortableTransport pins the
// declaration channel on the fallback transport: the declaration replaces the
// assessment, and the auditor answers through the prompt-only convention.
func TestRuntimeUpdateGoalCompletionIsAuditedOverThePortableTransport(t *testing.T) {
	t.Parallel()

	model := newGoalTransportModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeToolResponse("declare-1", tools.UpdateGoalName, `{"completed":true,"message":"the migration is done"}`),
		runtimeTextResponse("declared complete"),
	)
	runtime := openTestRuntime(t, model)
	writeGoalTransportProof(t, runtime)

	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 1, state.Attempts)

	declared := declarationResult(t, events, "declare-1")
	assert.False(t, declared.IsError, toolResultTextOf(t, declared))
	assert.Contains(t, toolResultTextOf(t, declared), "independent evidence audit")

	assessmentFormats, verdictFormats, _, verdictSystems := model.callFormats()
	assert.Empty(t, assessmentFormats, "the declaration replaced the assessment")
	require.Equal(t, []bool{false}, verdictFormats)
	assert.Contains(t, verdictSystems[0], "Native structured output is unavailable.")
}
