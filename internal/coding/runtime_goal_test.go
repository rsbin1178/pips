//nolint:wsl_v5 // Keep scripted model setup adjacent to behavioral assertions.
package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type goalTestModel struct {
	*runtimeModel
	muGoal sync.Mutex
	// decisions are the assessment's scripted decisions, one per assessment call;
	// once they run out, candidate_complete answers every further call.
	decisions []string
	// decisionDocuments are whole scripted assessment answers, one per assessment
	// call, for a test that needs a field the decision shortcut cannot express — a
	// blocked decision's blocker key. They take precedence over decisions.
	decisionDocuments []string
	evaluationError   bool
	blockEvaluation   chan struct{}
	evaluationStarted chan struct{}
	evaluated         int
	verifications     int
	// verifierVerdicts are the panel's scripted verdicts, one per verification
	// round applied to every skeptic of that round; once they run out,
	// verifierVerdict answers every further round. With neither, the panel returns
	// the default verdict: verified with the recorded evidence IDs and its own read.
	verifierVerdicts []string
	verifierVerdict  string
	// verifierPositionVerdicts overrides the verdict of one panel position — the
	// one-based position the skeptic's system prompt names — for every round, so a
	// test can script a split panel.
	verifierPositionVerdicts map[int]string
	// verifierFailure is returned by the verdict calls of the first
	// verifierFailureRounds verification rounds, so a test can pin an
	// infrastructure failure and then a successful resume.
	verifierFailure       error
	verifierFailureRounds int
	// auditorMessages records, per panel position, the message count of every
	// audit call that position made, so a test can pin that no skeptic sees
	// another's history.
	auditorMessages map[int][]int
	// auditorEvidence is the ledger the auditor's last verdict request carried,
	// so a test can assert what the audit was actually asked to judge.
	auditorEvidence goalflow.Evidence
	// assessments are the assessment requests this scripted model answered. They
	// are recorded here rather than in the work script, because this model answers
	// them itself; a test reads them to inspect the request body.
	assessments []ai.Request
}

// AssessmentRequests returns the assessment requests this model answered.
func (m *goalTestModel) AssessmentRequests() []ai.Request {
	m.muGoal.Lock()
	defer m.muGoal.Unlock()

	return append([]ai.Request(nil), m.assessments...)
}

// AuditorEvidence returns the ledger the auditor's last verdict request carried.
func (m *goalTestModel) AuditorEvidence() goalflow.Evidence {
	m.muGoal.Lock()
	defer m.muGoal.Unlock()

	return m.auditorEvidence
}

// AuditorMessages returns the message count of every audit call, per panel
// position, so a test can pin that the panel's skeptics never share a history.
func (m *goalTestModel) AuditorMessages() map[int][]int {
	m.muGoal.Lock()
	defer m.muGoal.Unlock()

	counts := make(map[int][]int, len(m.auditorMessages))
	for position, calls := range m.auditorMessages {
		counts[position] = append([]int(nil), calls...)
	}

	return counts
}

// auditorRequestEvidence extracts the ledger the auditor's request carried with
// the record IDs the default verdict cites. Only the payload message names a
// condition, so a tool envelope cannot masquerade as the ledger.
func auditorRequestEvidence(request ai.Request) (goalflow.Evidence, []string) {
	evidence := goalflow.Evidence{}
	references := []string{"read:audit-read"}

	for _, message := range request.Messages {
		var payload struct {
			Condition string            `json:"condition"`
			Evidence  goalflow.Evidence `json:"evidence"`
		}
		if json.Unmarshal([]byte(runtimeMessageText(message)), &payload) != nil || payload.Condition == "" {
			continue
		}

		evidence = payload.Evidence
		for _, record := range payload.Evidence.Records {
			references = append(references, record.ID)
		}
	}

	return evidence, references
}

// testPanelSize is the default verifier panel's size, which the runtime builds
// for every Goal.
const testPanelSize = 3

// panelBools returns one entry per skeptic per verification round, so a test can
// assert a per-call request shape against the panel without depending on call
// order inside a round.
func panelBools(value bool, rounds int) []bool {
	values := make([]bool, 0, rounds*testPanelSize)
	for range rounds * testPanelSize {
		values = append(values, value)
	}

	return values
}

// panelCaps returns the per-call output caps of rounds default panels.
func panelCaps(rounds int) []*int {
	caps := make([]*int, 0, rounds*testPanelSize)
	for range rounds * testPanelSize {
		caps = append(caps, nil)
	}

	return caps
}

// panelReadCaps returns the read rounds' caps for rounds default panels: each of
// the three skeptics makes one tool-carrying round before it stops reading.
func panelReadCaps(rounds int) []*int {
	caps := make([]*int, 0, rounds*testPanelSize*2)
	for range rounds * testPanelSize * 2 {
		caps = append(caps, nil)
	}

	return caps
}

// goalPanelPosition parses the panel position and size out of a skeptic's system
// prompt, so the scripted model can route a verdict by round and position rather
// than by call order under the parallel panel.
func goalPanelPosition(system string) (int, int) {
	var position, total int

	if _, err := fmt.Sscanf(system, "You are skeptic %d of %d ", &position, &total); err != nil {
		return 0, 0
	}

	return position, total
}

func newGoalTestModel(work ...*ai.Response) *goalTestModel {
	return &goalTestModel{runtimeModel: newRuntimeModel(work...)}
}

// goalAssessmentBody is the assessment's request body, which Phase AC made the
// reference's {objective, transcript, plan}.
type goalAssessmentBody struct {
	Objective  string `json:"objective"`
	Transcript string `json:"transcript"`
	Plan       string `json:"plan"`
}

// lastAssessmentBody decodes the body of the newest assessment request.
func lastAssessmentBody(t *testing.T, requests []ai.Request) goalAssessmentBody {
	t.Helper()

	require.NotEmpty(t, requests)

	_, conversation, err := requests[len(requests)-1].Messages.SplitSystem()
	require.NoError(t, err)
	require.Len(t, conversation, 1)

	body := goalAssessmentBody{}
	require.NoError(t, json.Unmarshal([]byte(runtimeMessageText(conversation[0])), &body))

	return body
}

// goalRefutingVerdict is a scripted auditor rejection: the recorded ledger does
// not prove the condition, so the loop continues with the auditor's own gap.
const goalRefutingVerdict = `{"verified":false,"reason":"the recorded evidence does not prove the condition","gaps":["record a check that proves the condition"],"references":[]}`

func (m *goalTestModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}

//nolint:nestif // The scripted model distinguishes independent request roles.
func (m *goalTestModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	if request.ResponseFormat != nil && request.ResponseFormat.Name == "goal_evaluation" {
		m.muGoal.Lock()
		m.evaluated++
		m.assessments = append(m.assessments, request)
		failure := m.evaluationError
		decision := "candidate_complete"
		if len(m.decisions) > 0 {
			decision = m.decisions[0]
			m.decisions = m.decisions[1:]
		}
		document := ""
		if len(m.decisionDocuments) > 0 {
			document = m.decisionDocuments[0]
			m.decisionDocuments = m.decisionDocuments[1:]
		}
		m.muGoal.Unlock()
		if m.evaluationStarted != nil {
			select {
			case m.evaluationStarted <- struct{}{}:
			default:
			}
		}
		if m.blockEvaluation != nil {
			select {
			case <-m.blockEvaluation:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if failure {
			return &ai.Response{Usage: ai.Usage{InputTokens: 5, OutputTokens: 2}}, errors.New("assessment unavailable")
		}
		if document != "" {
			return runtimeTextResponse(document), nil
		}
		// Phase AB's assessment document: the reference's four fields. A scripted
		// decision is always a continue or a candidate completion, so the blocker
		// key stays empty.
		return runtimeTextResponse(
			`{"decision":"` + decision + `","evidence":"inspect actual implementation",` +
				`"next_step":"inspect the actual implementation","blocker_key":""}`,
		), nil
	}
	if strings.Contains(requestSystemText(request), "independent Coding Goal evidence auditor") {
		position, total := goalPanelPosition(requestSystemText(request))

		m.muGoal.Lock()
		if m.auditorMessages == nil {
			m.auditorMessages = map[int][]int{}
		}
		m.auditorMessages[position] = append(m.auditorMessages[position], len(request.Messages))
		m.muGoal.Unlock()

		if request.ResponseFormat == nil {
			for _, message := range request.Messages {
				if _, ok := message.(ai.ToolMessage); ok {
					return runtimeTextResponse("evidence read"), nil
				}
			}
			return runtimeToolResponse("audit-read", "read", `{"path":"proof.txt"}`), nil
		}

		evidence, references := auditorRequestEvidence(request)

		// Every skeptic of one verification round answers the same scripted
		// verdict, keyed by the round index rather than by call order, so the
		// parallel panel is deterministic.
		m.muGoal.Lock()
		m.verifications++
		m.auditorEvidence = evidence
		round := (m.verifications - 1) / max(total, 1)
		scripted := m.verifierVerdict
		if verdict, ok := m.verifierPositionVerdicts[position]; ok {
			scripted = verdict
		} else if round < len(m.verifierVerdicts) {
			scripted = m.verifierVerdicts[round]
		}
		var failure error
		if round < m.verifierFailureRounds {
			failure = m.verifierFailure
		}
		m.muGoal.Unlock()

		if failure != nil {
			return nil, failure
		}

		if scripted != "" {
			return runtimeTextResponse(scripted), nil
		}

		data, _ := json.Marshal(goalflow.Verdict{Verified: true, Reason: "actual file matches captured read", Gaps: []string{}, References: references})
		return runtimeTextResponse(string(data)), nil
	}
	return m.runtimeModel.Generate(ctx, request)
}

func (m *goalTestModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
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

func TestGoalRequestValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		request GoalRequest
	}{
		{name: "empty", request: GoalRequest{}},
		{name: "oversize", request: GoalRequest{Condition: strings.Repeat("界", 4001)}},
		{name: "negative budget", request: GoalRequest{Condition: "done", MaxTokens: -1}},
		{name: "invalid text", request: GoalRequest{Condition: "bad\x00condition"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Error(t, test.request.Validate())
		})
	}
	require.NoError(t, (GoalRequest{Condition: strings.Repeat("界", 4000)}).Validate())
}

func TestRuntimeGoalContinuesThenVerifiesActualEvidence(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(
		runtimeTextResponse("need to inspect"),
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.decisions = []string{"continue", "candidate_complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))
	events, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)
	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 2, state.Attempts)
	assert.Equal(t, 2, state.Evaluations)
	assert.Positive(t, state.Tokens)
	assert.Equal(t, 3, model.verifications, "one default three-skeptic panel")
	assertEventSequence(t, events)
	assert.Contains(t, eventTypes(events), EventGoalChanged)
	assert.NotEmpty(t, runtime.Snapshot().SyntheticMessages)
	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.True(t, requestContainsText(requests[1], "Independent evidence gaps"))
}

func TestRuntimeGoalDecisionRetryDoesNotReplayWork(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	model := newGoalTestModel(runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`), runtimeTextResponse("candidate"))
	model.evaluationError = true
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.Error(t, err)
	first := runtime.GoalSnapshot()
	require.Equal(t, GoalInterrupted, first.Status)
	assert.Equal(t, 7, first.FailedCheckTokens)
	execution, err := runtime.goalControl.engine.Get(t.Context(), continuation.ID(first.ID))
	require.NoError(t, err)
	attemptID := execution.CurrentAttempt.ID
	sessionID := runtime.Snapshot().SessionID
	require.NoError(t, runtime.Close(t.Context()))
	reopenedModel := newGoalTestModel()
	reopened := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, reopenedModel)
	assert.Equal(t, GoalPaused, reopened.GoalSnapshot().Status)
	assert.Empty(t, reopenedModel.Requests())
	_, err = collectRuntimeResult(reopened.ResumeGoal(t.Context()))
	require.NoError(t, err)
	require.True(t, reopened.GoalSnapshot().Completed())
	assert.Equal(t, first.Attempts, reopened.GoalSnapshot().Attempts)
	assert.Equal(t, 7, reopened.GoalSnapshot().FailedCheckTokens)
	after, err := reopened.goalControl.engine.Get(t.Context(), continuation.ID(first.ID))
	require.NoError(t, err)
	require.NotNil(t, after.LastAttempt)
	assert.Equal(t, attemptID, after.LastAttempt.ID)
	assert.Empty(t, reopenedModel.Requests())
}

// TestRuntimeGoalRetriedDecisionCarriesTheTranscript pins the Phase AC seam's
// decisive property: a decision retried after an interruption rebuilds its request
// from durable state, so a transcript carried there would be empty; the wrapper
// reads the session instead, and the retried assessment still receives the
// conversation the first attempt worked in. It also pins the same-process decision
// — the one taken straight after the work turn — so the transcript is known to
// carry that turn at the instant it is read, not merely after a reload.
func TestRuntimeGoalRetriedDecisionCarriesTheTranscript(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := newGoalTestModel(runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`), runtimeTextResponse("candidate"))
	model.evaluationError = true
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.Error(t, err)
	require.Equal(t, GoalInterrupted, runtime.GoalSnapshot().Status)

	// The same-process decision — the one taken straight after the work turn,
	// before the interruption — already reads that turn, which is the property the
	// retry below preserves.
	require.Len(t, model.AssessmentRequests(), 1, "one assessment ran before the interruption")
	fresh := lastAssessmentBody(t, model.AssessmentRequests())
	assert.Contains(t, fresh.Transcript, "[user] inspect proof.txt")
	assert.Contains(t, fresh.Transcript, "[tool]", "the decision reads the tool result of the turn it just ran")
	assert.True(t, strings.HasSuffix(fresh.Transcript, "[assistant] candidate"),
		"the turn's own answer is the newest row, so the transcript is read at the decision instant: %q", fresh.Transcript)

	sessionID := runtime.Snapshot().SessionID
	require.NoError(t, runtime.Close(t.Context()))

	reopened := newGoalTestModel()
	restored := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, reopened)
	_, err = collectRuntimeResult(restored.ResumeGoal(t.Context()))
	require.NoError(t, err)
	require.True(t, restored.GoalSnapshot().Completed())

	body := lastAssessmentBody(t, reopened.AssessmentRequests())
	assert.Equal(t, "inspect proof.txt", body.Objective)
	assert.Equal(t, "(no plan available)", body.Plan)
	assert.Contains(t, body.Transcript, "[user] inspect proof.txt")
	assert.Contains(t, body.Transcript, "[tool]", "the retried assessment reads the work the first attempt recorded")
}

// TestRuntimeGoalTakesAVerifiedVerdictAsCompletion pins Phase AA's inverted
// contract: the machine proof gate is gone, so a verdict that says verified
// completes the Goal even when the recorded evidence is empty, failed or
// truncated, or when the verdict cites nothing. The auditor's prompt, not this
// harness, is what asks for proof now.
func TestRuntimeGoalTakesAVerifiedVerdictAsCompletion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// read is the path the worker's read round uses; "" makes the worker claim
		// completion without touching the workspace.
		read    string
		files   map[string]string
		verdict string
		// check asserts the ledger the auditor was handed, so no case can pass
		// without the recorded evidence its name claims.
		check func(*testing.T, goalflow.Evidence)
	}{
		{
			name: "self report", files: map[string]string{"proof.txt": "proof\n"},
			check: func(t *testing.T, evidence goalflow.Evidence) {
				t.Helper()

				assert.Empty(t, evidence.Records)
			},
		},
		{
			name: "empty verification", read: "proof.txt", files: map[string]string{"proof.txt": "proof\n"},
			verdict: `{"verified":true,"reason":"looks good","gaps":[],"references":[]}`,
			check: func(t *testing.T, evidence goalflow.Evidence) {
				t.Helper()

				require.Len(t, evidence.Records, 1)
				assert.True(t, evidence.Records[0].OK)
			},
		},
		{
			// The worker's read fails, so the ledger carries a failed record, while
			// the auditor's own read of proof.txt still succeeds.
			name: "failed evidence", read: "missing.txt", files: map[string]string{"proof.txt": "proof\n"},
			check: func(t *testing.T, evidence goalflow.Evidence) {
				t.Helper()

				require.Len(t, evidence.Records, 1)
				assert.False(t, evidence.Records[0].OK, "the read of a missing file must be a failed record")
			},
		},
		{
			// The worker's read is truncated by the read tool's own line limit, and
			// the auditor reads the small proof.txt, so the audit itself stays inside
			// its evidence envelope.
			name: "truncated evidence", read: "big.txt",
			files: map[string]string{"proof.txt": "proof\n", "big.txt": strings.Repeat("proof line\n", 2001)},
			check: func(t *testing.T, evidence goalflow.Evidence) {
				t.Helper()

				require.Len(t, evidence.Records, 1)
				assert.True(t, evidence.Records[0].Truncated, "a 2,001-line read must be a truncated record")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			work := []*ai.Response{}
			for attempt := range 4 {
				if test.read != "" {
					work = append(work, runtimeToolResponse(
						fmt.Sprintf("proof-read-%d", attempt), "read", fmt.Sprintf(`{"path":%q}`, test.read),
					))
				}
				work = append(work, runtimeTextResponse("I claim everything is verified"))
			}

			model := newGoalTestModel(work...)
			if test.verdict != "" {
				model.verifierVerdicts = []string{test.verdict}
			}
			runtime := openTestRuntime(t, model)
			for path, content := range test.files {
				require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), path), []byte(content), 0o600))
			}

			_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "prove implementation"}))
			require.NoError(t, err)

			state := runtime.GoalSnapshot()
			require.True(t, state.Completed(), "%+v", state)
			assert.Equal(t, GoalCompleted, state.Status)
			assert.Equal(t, 3, model.verifications, "one default three-skeptic panel")
			test.check(t, model.AuditorEvidence())
		})
	}
}

// TestRuntimeGoalKeepsRefusingARejectingOrMalformedVerdict pins what still stops
// the loop after Phase AA: a verdict that says verified:false with gaps keeps the
// Goal running, and a verdict that is not one JSON object (§6.33) is "not
// achieved" too. Either way the Goal pauses with the auditor's reason as its gap.
func TestRuntimeGoalKeepsRefusingARejectingOrMalformedVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		verdict string
		wantGap string
	}{
		{
			name:    "refuted verdict",
			verdict: `{"verified":false,"reason":"the recorded evidence does not show the implementation","gaps":["rerun the failing check"],"references":[]}`,
			wantGap: "rerun the failing check",
		},
		{
			name:    "malformed verdict",
			verdict: "not a JSON verdict object",
			wantGap: "not the required JSON verdict object",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			work := []*ai.Response{}
			for range 4 {
				work = append(work, runtimeTextResponse("I claim everything is verified"))
			}

			model := newGoalTestModel(work...)
			model.verifierVerdict = test.verdict
			runtime := openTestRuntime(t, model)
			require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))

			_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "prove implementation"}))
			require.NoError(t, err)

			state := runtime.GoalSnapshot()
			assert.False(t, state.Completed())
			assert.Equal(t, GoalPaused, state.Status)
			require.NotEmpty(t, state.Gaps)
			assert.Contains(t, strings.Join(state.Gaps, " "), test.wantGap)
			// Phase AD's gap guard pauses the Goal on the second identical
			// rejection instead of running unbounded.
			assert.Equal(t, 2, state.Attempts)
		})
	}
}

func TestRuntimeGoalNoProgressAndBudget(t *testing.T) {
	t.Parallel()
	t.Run("two identical rejected verifications", func(t *testing.T) {
		t.Parallel()
		// Phase AD replaced the three-unchanged-evidence guard with the
		// reference's gap guard: the same aggregate gaps on two consecutive
		// not-achieved verifications pause the Goal.
		model := newGoalTestModel(runtimeTextResponse("claim1"), runtimeTextResponse("claim2"))
		model.decisions = []string{"candidate_complete", "candidate_complete"}
		model.verifierVerdict = goalRefutingVerdict
		runtime := openTestRuntime(t, model)
		require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
		_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "done"}))
		require.NoError(t, err)
		state := runtime.GoalSnapshot()
		assert.Equal(t, GoalPaused, state.Status)
		assert.Equal(t, 2, state.Attempts)
		assert.Contains(t, state.Reason, "same gaps")
		assert.Contains(t, state.Reason, "/goal resume")
	})
	t.Run("work token overshoot", func(t *testing.T) {
		t.Parallel()
		model := newGoalTestModel(runtimeTextResponse("candidate"))
		runtime := openTestRuntime(t, model)
		_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "done", MaxTokens: 1}))
		require.NoError(t, err)
		assert.Equal(t, GoalLimited, runtime.GoalSnapshot().Status)
		assert.Equal(t, 1, runtime.GoalSnapshot().Attempts)
		assert.Equal(t, 0, model.evaluated)
	})
}

// TestRuntimeGoalResumeResetsTheGapStallGuard pins the reset: an explicit resume
// starts the verification counters again, so the same rejection after a resume is
// the first of a new run rather than a third identical one.
func TestRuntimeGoalResumeResetsTheGapStallGuard(t *testing.T) {
	t.Parallel()

	model := newGoalTestModel(
		runtimeTextResponse("claim1"), runtimeTextResponse("claim2"),
		runtimeTextResponse("claim3"), runtimeTextResponse("claim4"),
	)
	model.decisions = []string{"candidate_complete", "candidate_complete", "candidate_complete", "candidate_complete"}
	// The first two rounds reject identically and stall the Goal; after the resume
	// the third rejection starts the count again and the fourth round verifies.
	model.verifierVerdicts = []string{goalRefutingVerdict, goalRefutingVerdict, goalRefutingVerdict}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "done"}))
	require.NoError(t, err)
	require.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)

	_, err = collectRuntimeResult(runtime.ResumeGoal(t.Context()))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 4, state.Attempts)
}

// scriptDistinctRejections scripts work segments whose panels reject with a
// distinct gap each round, so the stall guard cannot fire before the run cap.
func scriptDistinctRejections(model *goalTestModel, rounds int) {
	for index := range rounds {
		model.responses = append(model.responses, runtimeTextResponse(fmt.Sprintf("claim-%d", index)))
		model.decisions = append(model.decisions, "candidate_complete")
		model.verifierVerdicts = append(model.verifierVerdicts,
			fmt.Sprintf(`{"verified":false,"reason":"still unproven","gaps":["gap-%d"],"references":[]}`, index))
	}
}

// TestRuntimeGoalPausesAtTheVerificationRunCap pins the reference's per-goal cap:
// the eleventh candidate completion does not spend another panel; the Goal pauses
// with a resume sentence. Work segments stay uncapped, so only the panel count
// reaches the bar.
func TestRuntimeGoalPausesAtTheVerificationRunCap(t *testing.T) {
	t.Parallel()

	const runCap = 10

	model := newGoalTestModel()
	// Ten panels run and reject with distinct gaps; the eleventh candidate
	// completion blocks before spending another panel.
	scriptDistinctRejections(model, runCap)
	model.responses = append(model.responses, runtimeTextResponse("claim-cap"))
	model.decisions = append(model.decisions, "candidate_complete")
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "done"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	assert.Equal(t, GoalPaused, state.Status)
	assert.Contains(t, state.Reason, "verification runs")
	assert.Contains(t, state.Reason, "/goal resume")
	assert.Equal(t, runCap*3, model.verifications, "ten panels ran; the eleventh candidate completion blocked instead")
	assert.Equal(t, runCap+1, state.Attempts, "the work segments themselves are uncapped")
}

// TestRuntimeGoalResumeResetsTheVerificationRunCap pins the reference's reset: an
// explicit resume starts the run count again, so the Goal spends another panel
// instead of blocking immediately.
func TestRuntimeGoalResumeResetsTheVerificationRunCap(t *testing.T) {
	t.Parallel()

	const runCap = 10

	model := newGoalTestModel()
	// Ten panels run and reject with distinct gaps; the eleventh candidate
	// completion blocks before spending another panel, and the resumed run's one
	// further candidate completion verifies.
	scriptDistinctRejections(model, runCap)
	model.responses = append(model.responses, runtimeTextResponse("claim-cap"))
	model.decisions = append(model.decisions, "candidate_complete")
	model.responses = append(model.responses, runtimeTextResponse("candidate"))
	model.decisions = append(model.decisions, "candidate_complete")

	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "done"}))
	require.NoError(t, err)
	require.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)

	_, err = collectRuntimeResult(runtime.ResumeGoal(t.Context()))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, runCap*3+3, model.verifications, "the resumed run spent one more panel")
	assert.Equal(t, runCap+2, state.Attempts)
}

// TestRuntimeGoalBlocksOnTheThirdSameAssessmentBlocker pins the projection the
// user sees when the assessment declares the same blocker three times running.
// Phase AB makes that a blocked Goal rather than a paused one — the reference
// pauses it as `Verification`, whose status is `Blocked` — and hands it the
// reference's message, the evidence followed by the next user action. The streak's
// own counting is pinned by the SDK controller's tests.
func TestRuntimeGoalBlocksOnTheThirdSameAssessmentBlocker(t *testing.T) {
	t.Parallel()

	const document = `{"decision":"blocked","evidence":"the deploy key is absent",` +
		`"next_step":"add DEPLOY_KEY to the CI environment","blocker_key":"missing_deploy_key"}`

	model := newGoalTestModel(
		runtimeTextResponse("claim1"), runtimeTextResponse("claim2"), runtimeTextResponse("claim3"),
	)
	model.decisionDocuments = []string{document, document, document}
	runtime := openTestRuntime(t, model)

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "the release is published"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	assert.Equal(t, GoalBlocked, state.Status)
	assert.Equal(t, 3, state.Evaluations)
	assert.Equal(t, 3, state.Attempts)
	assert.Equal(t, "the deploy key is absent\nNext user action: add DEPLOY_KEY to the CI environment", state.Reason)
}

func TestRuntimeGoalApprovalResolutionKeepsExactPendingCalls(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(
		runtimeToolResponse("pending-shell", "shell", `{"command":"printf ok","permissions":{"write_paths":[],"network":true},"justification":"test"}`),
		runtimeTextResponse("denied"), runtimeTextResponse("no evidence"), runtimeTextResponse("still none"), runtimeTextResponse("no new evidence"),
	)
	// Phase AD removed the three-unchanged-evidence guard, so the Goal now stops on
	// the verification gap guard instead: the scripted panels reject identically and
	// the Goal pauses on the second identical rejection.
	model.verifierVerdict = goalRefutingVerdict
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "run command"}))
	require.NoError(t, err)
	assert.Equal(t, GoalBlocked, runtime.GoalSnapshot().Status)
	assert.Equal(t, 0, model.evaluated)
	paused := runtime.Snapshot()
	require.NotNil(t, paused.Approval.Required)
	require.NoError(t, runtime.PauseGoal(t.Context()))
	_, err = collectRuntimeResult(runtime.Resolve(t.Context(), approval.Resolution{RequestID: paused.Approval.Required.RequestID, Choice: approval.ChoiceDeny}))
	require.ErrorIs(t, err, ErrRuntimePending)
	assert.Len(t, model.Requests(), 1)
	_, err = collectRuntimeResult(runtime.ResumeGoal(t.Context()))
	require.NoError(t, err)
	require.Equal(t, GoalBlocked, runtime.GoalSnapshot().Status)
	_, err = collectRuntimeResult(runtime.Resolve(t.Context(), approval.Resolution{RequestID: paused.Approval.Required.RequestID, Choice: approval.ChoiceDeny}))
	require.NoError(t, err)
	state := runtime.GoalSnapshot()
	assert.Equal(t, GoalPaused, state.Status)
	assert.Contains(t, state.Reason, "same gaps")
	assert.Equal(t, 3, state.Attempts)
	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.Equal(t, 1, countUserMessages(requests[1].Messages))
	results := 0
	for _, entry := range runtime.session.Entries() {
		if message, ok := entry.Message.(ai.ToolMessage); ok {
			for _, part := range message.Parts {
				if part.ToolCallID == "pending-shell" {
					results++
				}
			}
		}
	}
	assert.Equal(t, 1, results)
}

func TestRuntimeGoalWaitUsesNotificationAndCannotRearmPausedGoal(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(runtimeTextResponse("waiting for child"), runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`), runtimeTextResponse("candidate"))
	runtime := openTestRuntime(t, model)
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("proof\n"), 0o600))
	runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"})(func(event Event, err error) bool {
		require.NoError(t, err)
		if event.Type == EventInteractionCompleted {
			require.NoError(t, runtime.notifications.Enqueue(testAgentNotification(runtime, "s-goal-child", event.InteractionID)))
		}
		return true
	})
	require.Equal(t, GoalWaiting, runtime.GoalSnapshot().Status)
	assert.Equal(t, 0, model.evaluated)
	require.NoError(t, runtime.PauseGoal(t.Context()))
	runtime.deliverNotificationBatch(t.Context())
	assert.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)
	assert.Len(t, model.Requests(), 1)
	_, err := collectRuntimeResult(runtime.ResumeGoal(t.Context()))
	require.NoError(t, err)
	runtime.deliverNotificationBatch(t.Context())
	require.True(t, runtime.GoalSnapshot().Completed(), "%+v", runtime.GoalSnapshot())
	assert.Equal(t, 2, runtime.GoalSnapshot().Attempts)
	pending, err := runtime.notifications.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// TestRuntimeGoalRunsPastTheOldSegmentCap pins Phase Z: the Goal carries no
// work-segment cap, so a Goal that keeps producing new evidence runs past the 25
// segments the continuation default used to allow and stops by its token budget
// rather than by a segment count.
func TestRuntimeGoalRunsPastTheOldSegmentCap(t *testing.T) {
	t.Parallel()

	const segments = 27

	model := newGoalTestModel()
	runtime := openTestRuntime(t, model)

	// Every scripted response reports the same usage, and the goal test model
	// answers the assessment with one too. A work segment is two work turns plus
	// that assessment: the turns report their run's cumulative usage (1x and 2x),
	// so a segment costs four scripted responses' worth of observed tokens and the
	// budget below pays for exactly `segments` of them.
	response := runtimeTextResponse("continue")
	segmentTokens := 4 * (response.Usage.InputTokens + response.Usage.OutputTokens)

	for index := range segments {
		name := fmt.Sprintf("proof-%d.txt", index)
		require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), name), []byte(name), 0o600))
		model.responses = append(model.responses, runtimeToolResponse(fmt.Sprintf("call-%d", index), "read", fmt.Sprintf(`{"path":%q}`, name)), runtimeTextResponse("continue"))
		model.decisions = append(model.decisions, "continue")
	}

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{
		Condition: "keep inspecting", MaxTokens: segments * segmentTokens,
	}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	assert.Equal(t, GoalLimited, state.Status)
	// 27 segments ran; the removed default stopped the Goal at 25.
	assert.Equal(t, segments, state.Attempts)
	assert.Equal(t, segments, state.Evaluations)
	assert.Len(t, model.Requests(), segments*2)
}

// TestRuntimeGoalWorkSegmentIsNotCappedByTheRemainingBudget pins Phase Z: the
// work interaction installs no cap derived from the Goal's remaining budget, so
// a work segment reaches its own answer even when the budget cannot pay for it,
// and the budget keeps its post-hoc check at the loop boundary.
func TestRuntimeGoalWorkSegmentIsNotCappedByTheRemainingBudget(t *testing.T) {
	t.Parallel()

	model := newGoalTestModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.decisions = []string{"candidate_complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))

	// One scripted response's worth of budget: a per-call cap would have stopped
	// the work segment after its first turn.
	response := runtimeTextResponse("continue")
	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{
		Condition: "inspect proof.txt", MaxTokens: response.Usage.InputTokens + response.Usage.OutputTokens,
	}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	assert.Equal(t, GoalLimited, state.Status)
	assert.Equal(t, 1, state.Attempts)
	// Both work turns ran, so the segment ended on the model's own answer instead
	// of being cut off mid-segment, and the budget stopped the Goal before the
	// assessment.
	assert.Len(t, model.Requests(), 2)
	assert.Equal(t, 0, state.Evaluations)
}

// TestRuntimeGoalWorkRunsWithoutATurnCap pins Phase M: a goal work interaction
// installs no turn cap, so it is bounded only by the goal's token budget, as in
// the reference. With the old goalWorkMaxTurns = 16 the run stopped at turn 16
// and never sent its final answer.
func TestRuntimeGoalWorkRunsWithoutATurnCap(t *testing.T) {
	t.Parallel()

	const workTurns = 18
	model := newGoalTestModel()
	for turn := range workTurns - 1 {
		model.responses = append(model.responses, runtimeToolResponse(
			fmt.Sprintf("proof-read-%d", turn), "read", `{"path":"proof.txt"}`,
		))
	}
	model.responses = append(model.responses, runtimeTextResponse("candidate complete"))
	model.decisions = []string{"candidate_complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(
		filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600,
	))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	// Every work turn ran, including the final answer past the old 16th turn.
	assert.Len(t, model.Requests(), workTurns)
	require.True(t, runtime.GoalSnapshot().Completed(), "%+v", runtime.GoalSnapshot())
}

func TestRuntimeGoalPauseDuringAssessmentStartsNoAdditionalWork(t *testing.T) {
	t.Parallel()
	model := newGoalTestModel(runtimeTextResponse("candidate"))
	model.evaluationStarted = make(chan struct{}, 1)
	model.blockEvaluation = make(chan struct{})
	runtime := openTestRuntime(t, model)
	done := make(chan error, 1)
	go func() {
		_, err := collectRuntimeResult(runtime.StartGoal(context.Background(), GoalRequest{Condition: "done"}))
		done <- err
	}()
	<-model.evaluationStarted
	require.NoError(t, runtime.PauseGoal(t.Context()))
	<-done
	assert.Equal(t, GoalPaused, runtime.GoalSnapshot().Status)
	assert.Len(t, model.Requests(), 1)
	assert.Equal(t, 1, runtime.GoalSnapshot().Attempts)
	require.NoError(t, runtime.ClearGoal(t.Context()))
	assert.Empty(t, runtime.GoalSnapshot().ID)
}

// TestRuntimeGoalPanelColdMajorityAcceptsDespiteTheGatekeeper pins Phase AD's
// loosest designed case: the gatekeeper's lone refute is one vote, and the two
// cold skeptics carry the completion.
func TestRuntimeGoalPanelColdMajorityAcceptsDespiteTheGatekeeper(t *testing.T) {
	t.Parallel()

	model := newGoalTestModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.decisions = []string{"candidate_complete"}
	// Position one is the gatekeeper, whose non-refute never counts and whose
	// refute is only one vote.
	model.verifierPositionVerdicts = map[int]string{1: goalRefutingVerdict}
	// Three further rounds exist only so a regression that lets the gatekeeper's
	// refute veto has somewhere to go: the identical rejection then trips the gap
	// guard and the Goal pauses, so this test's own completion assertion is what
	// fails rather than its fixture running out of script.
	model.responses = append(model.responses,
		runtimeTextResponse("candidate complete"),
		runtimeTextResponse("candidate complete"),
		runtimeTextResponse("candidate complete"),
	)
	model.decisions = append(model.decisions, "candidate_complete", "candidate_complete", "candidate_complete")
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.True(t, state.Completed(), "%+v", state)
	assert.Equal(t, 1, state.Attempts)
	assert.Equal(t, 3, model.verifications)
	assert.Empty(t, state.Gaps, "the achieved panel returns the accepting skeptic's own document")
}

// TestRuntimeGoalPanelSkepticsDoNotShareContext pins Phase AD's independence: the
// three skeptics of one round each answer from their own message history, so no
// call ever sees another skeptic's read or answer. A shared history would grow
// past the two-read-then-verdict shape every skeptic has.
func TestRuntimeGoalPanelSkepticsDoNotShareContext(t *testing.T) {
	t.Parallel()

	model := newGoalTestModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.decisions = []string{"candidate_complete"}
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)
	require.True(t, runtime.GoalSnapshot().Completed(), "%+v", runtime.GoalSnapshot())

	counts := model.AuditorMessages()
	require.Len(t, counts, 3, "one history per skeptic")

	for position, calls := range counts {
		// system+payload, then the tool call and its result, then the skeptic's own
		// answer, then the verdict prompt: a skeptic that saw another's turn would
		// carry more.
		assert.Equal(t, []int{2, 4, 6}, calls, "skeptic %d", position)
	}
}

// TestRuntimeGoalVerifierInfrastructureFailurePausesFailClosed pins Phase AD's
// infrastructure rule: a skeptic model failure pauses the Goal with the
// reference's sentence instead of failing the Decision, and an explicit resume
// runs the verification again.
func TestRuntimeGoalVerifierInfrastructureFailurePausesFailClosed(t *testing.T) {
	t.Parallel()

	model := newGoalTestModel(
		runtimeToolResponse("proof-read", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
		runtimeToolResponse("proof-read-2", "read", `{"path":"proof.txt"}`),
		runtimeTextResponse("candidate complete"),
	)
	model.verifierFailure = errors.New("provider unavailable")
	model.verifierFailureRounds = 1
	runtime := openTestRuntime(t, model)
	require.NoError(t, os.WriteFile(filepath.Join(runtime.workspace.Root(), "proof.txt"), []byte("actual implementation\n"), 0o600))

	_, err := collectRuntimeResult(runtime.StartGoal(t.Context(), GoalRequest{Condition: "inspect proof.txt"}))
	require.NoError(t, err)

	state := runtime.GoalSnapshot()
	require.Equal(t, GoalPaused, state.Status, "%+v", state)
	assert.False(t, state.Completed())
	assert.Contains(t, state.Reason,
		"Goal verification infrastructure failed (provider unavailable). Run /goal resume to retry.")

	// The pause is fail-closed but resumable: the next round's panel verifies.
	model.muGoal.Lock()
	model.verifierFailureRounds = 0
	model.muGoal.Unlock()

	_, err = collectRuntimeResult(runtime.ResumeGoal(t.Context()))
	require.NoError(t, err)
	require.True(t, runtime.GoalSnapshot().Completed(), "%+v", runtime.GoalSnapshot())
}
