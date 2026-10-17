package goalflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingSpawner is a scripted SkepticSpawner: it records every request and
// answers by panel position, so a unit test can pin the spawn seam, the packet
// it renders and the fallback chain without a real child agent.
type recordingSpawner struct {
	mu        sync.Mutex
	requests  []SkepticRequest
	terminals map[int]string
	usage     ai.Usage
	failure   error
	transport error
}

func (spawner *recordingSpawner) Spawn(_ context.Context, request SkepticRequest) (SkepticResult, error) {
	spawner.mu.Lock()
	spawner.requests = append(spawner.requests, request)
	spawner.mu.Unlock()

	if spawner.transport != nil {
		return SkepticResult{}, SkepticTransportFailure(spawner.transport)
	}

	if spawner.failure != nil {
		return SkepticResult{}, spawner.failure
	}

	terminal, scripted := spawner.terminals[request.Index+1]
	if !scripted {
		terminal = verifiedSkepticDocument
	}

	return SkepticResult{
		Terminal:       terminal,
		ChildSessionID: fmt.Sprintf("child-%d", request.Index+1),
		Usage:          spawner.usage,
	}, nil
}

func (spawner *recordingSpawner) recorded() []SkepticRequest {
	spawner.mu.Lock()
	defer spawner.mu.Unlock()

	return append([]SkepticRequest(nil), spawner.requests...)
}

// requestFor returns the request one panel position made.
func requestFor(t *testing.T, spawner *recordingSpawner, position int) SkepticRequest {
	t.Helper()

	for _, request := range spawner.recorded() {
		if request.Index+1 == position {
			return request
		}
	}

	require.Failf(t, "missing panel position", "skeptic %d never spawned", position)

	return SkepticRequest{}
}

// spawnedVerifier builds a panel over a scratch workspace whose skeptics are
// scripted spawns rather than in-process audits.
func spawnedVerifier(t *testing.T, spawner SkepticSpawner, options ...VerifierOption) *Verifier {
	t.Helper()

	_, tree := auditWorkspace(t)

	verifier, err := NewVerifier(t.Context(), &auditScriptModel{failure: errUnexpectedAuditCall}, tree,
		append([]VerifierOption{WithSpawner(spawner)}, options...)...)
	require.NoError(t, err)

	return verifier
}

// errUnexpectedAuditCall fails the in-process auditor, so a test that configures
// a spawner proves the panel never fell back to it.
var errUnexpectedAuditCall = errors.New("the in-process auditor must not run")

// TestVerifierSpawnsThePanelWithTheEvidencePacket pins the spawn seam: a
// configured spawner runs every skeptic, each one's task carries the reference's
// packet sections, and the in-process auditor never runs.
func TestVerifierSpawnsThePanelWithTheEvidencePacket(t *testing.T) {
	t.Parallel()

	spawner := &recordingSpawner{usage: ai.Usage{InputTokens: 5, OutputTokens: 1}}
	verifier := spawnedVerifier(t, spawner, WithChanges(func(context.Context) Changes {
		return Changes{Diff: "diff --git a/proof.txt b/proof.txt\n+actual implementation\n", ChangedFiles: []string{"proof.txt"}}
	}))

	verdict, usage, err := verifier.Verify(t.Context(), Packet{
		Objective: "the migration ran", FinalResponse: "I ran the migration", PriorGaps: []string{"rerun the check"},
	}, auditEvidence())
	require.NoError(t, err)
	require.True(t, verdict.Verified, verdict.Reason)
	assert.Equal(t, ai.Usage{InputTokens: 15, OutputTokens: 3}, usage)

	requests := spawner.recorded()
	require.Len(t, requests, defaultSkeptics)

	for index := range defaultSkeptics {
		request := requestFor(t, spawner, index+1)
		assert.Equal(t, index, request.Index)
		assert.Equal(t, defaultSkeptics, request.Total)
		assert.Empty(t, request.ResumeFrom, "the first verification resumes nobody")
		assert.Contains(t, request.Prompt, "You are skeptic "+strconv.Itoa(index+1)+" of 3")
		assert.Contains(t, request.Prompt, "OBJECTIVE:\nthe migration ran")
		assert.Contains(t, request.Prompt, "CHANGES_FILE:\ndiff --git a/proof.txt b/proof.txt")
		assert.Contains(t, request.Prompt, "CHANGED_FILES:\n- proof.txt")
		assert.Contains(t, request.Prompt, "PLAN_FILE: (unavailable)")
		assert.Contains(t, request.Prompt, "PLAN_CHANGES: (none)")
		assert.Contains(t, request.Prompt, "FINAL_RESPONSE:\nI ran the migration")
		assert.Contains(t, request.Prompt, "PRIOR_GAPS:\nrerun the check")
		assert.Contains(t, request.Prompt, verifierToolset)
		assert.Contains(t, request.Prompt, verifierOutputContract)
		assert.NotContains(t, request.Prompt, "You are the SAME adversarial verifier")
	}
}

// TestVerifierPacketRendersTheReferenceSentinels pins the packet's degraded
// shape: an uncaptured change set renders the reference's own markers rather
// than an empty section.
func TestVerifierPacketRendersTheReferenceSentinels(t *testing.T) {
	t.Parallel()

	spawner := &recordingSpawner{}
	verifier := spawnedVerifier(t, spawner)

	_, _, err := verifier.Verify(t.Context(), Packet{Objective: "the migration ran"}, auditEvidence())
	require.NoError(t, err)

	prompt := requestFor(t, spawner, 1).Prompt
	assert.Contains(t, prompt, "CHANGES_FILE: (unavailable)")
	assert.Contains(t, prompt, "CHANGED_FILES:\n(none captured)")
	assert.Contains(t, prompt, "PLAN_FILE: (unavailable)")
	assert.Contains(t, prompt, "PLAN_CHANGES: (none)")
	assert.Contains(t, prompt, "PRIOR_GAPS:\n(none)")
}

// TestVerifierResumesOnlyTheGatekeeperOfALargerPanel pins the resume contract: a
// panel larger than one resumes its first skeptic with the resume prompt, and a
// one-skeptic panel never resumes its sole judge.
func TestVerifierResumesOnlyTheGatekeeperOfALargerPanel(t *testing.T) {
	t.Parallel()

	spawner := &recordingSpawner{}
	verifier := spawnedVerifier(t, spawner)

	verdict, _, err := verifier.Verify(t.Context(), Packet{
		Objective: "the migration ran", PriorSkeptic0: "child-source",
	}, auditEvidence())
	require.NoError(t, err)

	gatekeeper := requestFor(t, spawner, 1)
	assert.Equal(t, "child-source", gatekeeper.ResumeFrom)
	assert.Contains(t, gatekeeper.Prompt, "You are the SAME adversarial verifier from the previous attempt")
	assert.Equal(t, "child-1", verdict.Skeptic0Session, "the panel records the gatekeeper's session")

	for _, position := range []int{2, 3} {
		assert.Empty(t, requestFor(t, spawner, position).ResumeFrom)
	}

	single := &recordingSpawner{}
	singleVerifier := spawnedVerifier(t, single, WithSkeptics(1))

	verdict, _, err = singleVerifier.Verify(t.Context(), Packet{
		Objective: "the migration ran", PriorSkeptic0: "child-source",
	}, auditEvidence())
	require.NoError(t, err)

	request := requestFor(t, single, 1)
	assert.Empty(t, request.ResumeFrom, "a sole judge is never the biased approver")
	assert.NotContains(t, request.Prompt, "You are the SAME adversarial verifier")
	assert.Empty(t, verdict.Skeptic0Session, "a one-skeptic panel records no session")
}

// TestVerifierSpawnFailureIsASkepticFailure pins the failure split: one
// skeptic's spawn or child failure becomes that skeptic's synthetic refute,
// never the panel's infrastructure pause.
func TestVerifierSpawnFailureIsASkepticFailure(t *testing.T) {
	t.Parallel()

	spawner := &recordingSpawner{failure: errors.New("the child session could not start")}
	verifier := spawnedVerifier(t, spawner, WithSkeptics(3))

	verdict, _, err := verifier.Verify(t.Context(), Packet{Objective: "the migration ran"}, auditEvidence())
	require.NoError(t, err, "a spawn failure is not a panel failure")
	require.False(t, verdict.Verified)
	assert.Contains(t, strings.Join(verdict.Gaps, " "), "skeptic failure")
	assert.Contains(t, strings.Join(verdict.Gaps, " "), "the child session could not start")
	assert.False(t, verdict.Blocked, "a synthetic refute carries no blocker")
}

// TestVerifierTransportFailureIsInfrastructure pins the other half: a failure of
// the child transport itself pauses the Goal fail-closed instead of voting.
func TestVerifierTransportFailureIsInfrastructure(t *testing.T) {
	t.Parallel()

	spawner := &recordingSpawner{transport: errors.New("no integration generation")}
	verifier := spawnedVerifier(t, spawner)

	_, _, err := verifier.Verify(t.Context(), Packet{Objective: "the migration ran"}, auditEvidence())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Goal verification infrastructure failed (the skeptic child transport failed: no integration generation)")
}

// TestVerifierReportsTheSpawnedPanel pins the diagnostic panel report: the sink
// receives one record per skeptic with the child session it ran in, and it is
// never consulted for the in-process fallback.
func TestVerifierReportsTheSpawnedPanel(t *testing.T) {
	t.Parallel()

	var reports [][]SkepticReport

	spawner := &recordingSpawner{terminals: map[int]string{
		2: `{"refuted":true,"evidence":"the migration is unproven","confidence":"medium","blocking":"none",` +
			`"findings":[{"kind":"gap","location":"migrate.go:12","detail":"rerun the check"}],"details_md":"## details"}`,
	}}
	verifier := spawnedVerifier(t, spawner, WithPanelReport(func(_ context.Context, records []SkepticReport) {
		reports = append(reports, records)
	}))

	verdict, _, err := verifier.Verify(t.Context(), Packet{Objective: "the migration ran"}, auditEvidence())
	require.NoError(t, err)
	require.False(t, verdict.Verified)

	require.Len(t, reports, 1)
	require.Len(t, reports[0], 3)
	assert.Equal(t, "child-1", reports[0][0].ChildSessionID)
	assert.False(t, reports[0][0].Refuted)
	assert.True(t, reports[0][1].Refuted)
	assert.Equal(t, "## details", reports[0][1].DetailsMD)
	assert.Equal(t, "child-3", reports[0][2].ChildSessionID)
}

// TestVerifierFallsBackToTheInProcessAudit pins the fallback: without a spawner
// the panel asks the model itself, and a sink is never consulted.
func TestVerifierFallsBackToTheInProcessAudit(t *testing.T) {
	t.Parallel()

	_, tree := auditWorkspace(t)
	model := &panelScriptModel{}
	consulted := false

	verifier, err := NewVerifier(t.Context(), model, tree, WithPanelReport(func(context.Context, []SkepticReport) {
		consulted = true
	}))
	require.NoError(t, err)

	verdict, _, err := verifier.Verify(t.Context(), Packet{Objective: "the migration ran"}, auditEvidence())
	require.NoError(t, err)
	require.True(t, verdict.Verified, verdict.Reason)
	assert.NotEmpty(t, model.prompts, "the in-process panel asked the model")
	assert.False(t, consulted, "the in-process panel writes no spawned-panel report")
	assert.Empty(t, verdict.Skeptic0Session)
}

// TestVerifierInProcessPromptCarriesThePacketSections pins the fallback's packet
// input: the ledger payload carries the change evidence the ledger itself does
// not, and stays unchanged when there is none.
func TestVerifierInProcessPromptCarriesThePacketSections(t *testing.T) {
	t.Parallel()

	_, tree := auditWorkspace(t)
	model := &auditScriptModel{rounds: []*ai.Response{
		{Message: ai.AssistantText("no further reads"), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}},
		{Message: ai.AssistantText(verifiedSkepticDocument), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}},
	}}

	verifier, err := NewVerifier(t.Context(), model, tree, WithSkeptics(1), WithChanges(func(context.Context) Changes {
		return Changes{Diff: "diff --git a/a b/a\n", ChangedFiles: []string{"a"}}
	}))
	require.NoError(t, err)

	_, _, err = verifier.Verify(t.Context(), Packet{Objective: "the migration ran"}, auditEvidence())
	require.NoError(t, err)
	require.NotEmpty(t, model.requests)
	assert.Contains(t, runtimeMessageTextOf(model.requests[0]), `"packet":"OBJECTIVE:`)
	assert.Contains(t, runtimeMessageTextOf(model.requests[0]), "CHANGED_FILES:")

	plain := &auditScriptModel{rounds: []*ai.Response{
		{Message: ai.AssistantText("no further reads"), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}},
		{Message: ai.AssistantText(verifiedSkepticDocument), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}},
	}}

	plainVerifier, err := NewVerifier(t.Context(), plain, tree, WithSkeptics(1))
	require.NoError(t, err)

	_, _, err = plainVerifier.Verify(t.Context(), Packet{Objective: "the migration ran"}, auditEvidence())
	require.NoError(t, err)
	require.NotEmpty(t, plain.requests)
	assert.NotContains(t, runtimeMessageTextOf(plain.requests[0]), `"packet"`)
}

// runtimeMessageTextOf joins every message's text, so a test can inspect the
// payload the panel actually sent.
func runtimeMessageTextOf(request ai.Request) string {
	var text strings.Builder

	for _, message := range request.Messages {
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}

		for _, part := range parts {
			if value, ok := part.(ai.TextPart); ok {
				text.WriteString(value.Text)
			}
		}
	}

	return text.String()
}

// TestRenderPacketBoundsTheChangedFileList pins the packet's own bound: 300
// rendered paths plus the reference's elision marker.
func TestRenderPacketBoundsTheChangedFileList(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 300, changedFilesMax, "the reference's own changed-file bound")

	files := make([]string, 305)
	for index := range files {
		files[index] = fmt.Sprintf("file-%03d.go", index)
	}

	section := renderPacket(Packet{Objective: "the migration ran", ChangedFiles: files})
	assert.Contains(t, section, "- file-000.go")
	assert.Contains(t, section, "- file-299.go")
	assert.NotContains(t, section, "- file-300.go")
	assert.Contains(t, section, "(… and 5 more)")
}

// TestRenderPacketMarksATruncatedDiff pins the diff's own bound marker.
func TestRenderPacketMarksATruncatedDiff(t *testing.T) {
	t.Parallel()

	section := renderPacket(Packet{Objective: "the migration ran", Diff: "diff --git a/a b/a\n", DiffTruncated: true})
	assert.Contains(t, section, "CHANGES_FILE:\ndiff --git a/a b/a\n")
	assert.Contains(t, section, "[the diff is truncated at the capture bound]")
}

// TestRenderPacketKeepsTheChildTaskValid pins the packet's task safety: the
// workspace- and model-derived sections carry no control rune a spawn request
// rejects, so a diff, a path or a response can never make the task itself
// invalid and turn every skeptic into a synthetic refute.
func TestRenderPacketKeepsTheChildTaskValid(t *testing.T) {
	t.Parallel()

	task := skepticTask(0, 3, Packet{
		Objective: "run the\x00 migration", Diff: "diff --git a/a b/a\n+line\x1b[31m\n",
		ChangedFiles: []string{"a\x1bb.go"}, FinalResponse: "done\x07",
		PriorGaps: []string{"fix\x0cthis"},
	}, false)

	for _, character := range task {
		if character < 0x20 && character != '\n' && character != '\r' && character != '\t' {
			require.Failf(t, "control rune in the child task", "%#U", character)
		}
	}

	assert.Contains(t, task, "+line\uFFFD[31m")
	assert.Contains(t, task, "- a\uFFFDb.go")
	assert.Contains(t, task, "fix\uFFFDthis")
}

// TestVerifierInProcessPayloadFallsBackWhenThePacketExceedsTheBound pins the
// packet's own bound against the persisted payload bound: a captured diff larger
// than that bound drops the packet section rather than failing the Decision.
func TestVerifierInProcessPayloadFallsBackWhenThePacketExceedsTheBound(t *testing.T) {
	t.Parallel()

	_, tree := auditWorkspace(t)
	model := &auditScriptModel{rounds: []*ai.Response{
		{Message: ai.AssistantText("no further reads"), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}},
		{Message: ai.AssistantText(verifiedSkepticDocument), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}},
	}}

	verifier, err := NewVerifier(t.Context(), model, tree, WithSkeptics(1), WithChanges(func(context.Context) Changes {
		return Changes{Diff: "diff --git a/a b/a\n" + strings.Repeat("x", MaxEvidenceBytes)}
	}))
	require.NoError(t, err)

	verdict, _, err := verifier.Verify(t.Context(), Packet{Objective: "the migration ran"}, auditEvidence())
	require.NoError(t, err, "a packet larger than the payload bound must not fail the Decision")
	require.True(t, verdict.Verified, verdict.Reason)
	require.NotEmpty(t, model.requests)
	assert.NotContains(t, runtimeMessageTextOf(model.requests[0]), "CHANGED_FILES:")
}

// TestFirstJSONObjectExtractsTheFirstBalancedObject pins the child answer's
// decode: the first balanced object is extracted, respecting string literals and
// their escapes.
func TestFirstJSONObjectExtractsTheFirstBalancedObject(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{name: "plain object", text: `{"refuted":false}`, want: `{"refuted":false}`, ok: true},
		{name: "object after prose", text: "Here is my verdict.\n{\"refuted\":true}\nRefuted", want: `{"refuted":true}`, ok: true},
		{
			name: "braces inside a string",
			text: `{"evidence":"a { brace } and a \" quote","refuted":false}`,
			want: `{"evidence":"a { brace } and a \" quote","refuted":false}`, ok: true,
		},
		{
			name: "an escaped backslash before a quote",
			text: `{"evidence":"ends with a backslash \\","refuted":true}`,
			want: `{"evidence":"ends with a backslash \\","refuted":true}`, ok: true,
		},
		{name: "nested objects", text: `{"a":{"b":1}}`, want: `{"a":{"b":1}}`, ok: true},
		{name: "unbalanced", text: `{"refuted":true`, ok: false},
		{name: "no object", text: "Not Refuted", ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			object, ok := firstJSONObject(test.text)
			assert.Equal(t, test.ok, ok)
			assert.Equal(t, test.want, object)
		})
	}
}

// TestVerdictFromTerminalUsesTheReferenceChain pins the spawned skeptic's decode
// chain: the JSON object first, the terminal token next, a synthetic refute last.
func TestVerdictFromTerminalUsesTheReferenceChain(t *testing.T) {
	t.Parallel()

	decoded := verdictFromTerminal(`{"refuted":true,"evidence":"the migration is unproven","confidence":"high",` +
		`"blocking":"contradiction","findings":[],"details_md":""}`)
	require.True(t, decoded.Refuted)
	assert.Equal(t, ConfidenceHigh, decoded.Confidence)
	assert.Equal(t, BlockingContradiction, decoded.Blocking)

	token := verdictFromTerminal("```\nNot Refuted\n```")
	require.False(t, token.Refuted)
	assert.Equal(t, ConfidenceUnknown, token.Confidence)
	assert.Equal(t, "verdict JSON missing/malformed; used terminal token", token.Evidence)

	// Prose around the token is not the reference's contract, so the answer
	// fails closed rather than reading a token out of a sentence.
	prose := verdictFromTerminal("I could not find the recorded evidence.\n\nNot Refuted")
	require.True(t, prose.Refuted)
	assert.Contains(t, prose.Evidence, "skeptic failure")

	// A document that breaks the contract is unusable, so the token decides.
	unusable := verdictFromTerminal(`{"refuted":true}` + "\nRefuted")
	require.True(t, unusable.Refuted)
	assert.Equal(t, ConfidenceUnknown, unusable.Confidence)

	synthetic := verdictFromTerminal("I could not decide")
	require.True(t, synthetic.Refuted)
	assert.Equal(t, ConfidenceUnknown, synthetic.Confidence)
	assert.Contains(t, synthetic.Evidence, "skeptic failure")
}

// TestSkepticTaskKeepsThePhaseAERules pins the spawned child's contract: the
// reference's judgment rules, pips' own tool names, the packet inputs and the
// output contract all ride the child's task.
func TestSkepticTaskKeepsThePhaseAERules(t *testing.T) {
	t.Parallel()

	task := skepticTask(0, 3, Packet{Objective: "the migration ran"}, false)
	assert.True(t, strings.HasPrefix(task, "You are skeptic 1 of 3 on an independent Coding Goal evidence auditor panel."), task)
	assert.Contains(t, task, verifierSystem)
	assert.Contains(t, task, "## Inputs")
	assert.Contains(t, task, verifierOutputContract)
	assert.Contains(t, task, "OBJECTIVE:\nthe migration ran")
	assert.NotContains(t, task, "You are the SAME adversarial verifier")

	resumed := skepticTask(0, 3, Packet{Objective: "the migration ran"}, true)
	assert.Contains(t, resumed, "You are the SAME adversarial verifier from the previous attempt")
	assert.Contains(t, resumed, verifierSystem)
	assert.Contains(t, resumed, "RE-CHECK")
}
