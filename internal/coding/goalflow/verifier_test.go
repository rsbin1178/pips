package goalflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/goal"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditScriptModel answers the auditor's read round and then its verdict, so a
// unit test can pin the verdict's own document contract without a work driver.
type auditScriptModel struct {
	rounds []*ai.Response
}

func (model *auditScriptModel) Generate(_ context.Context, _ ai.Request) (*ai.Response, error) {
	if len(model.rounds) == 0 {
		return nil, errors.New("audit script exhausted")
	}

	response := model.rounds[0]
	model.rounds = model.rounds[1:]

	return response, nil
}

func (*auditScriptModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(func(ai.StreamEvent, error) bool) {}
}

func (*auditScriptModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*auditScriptModel) ModelID() string       { return "audit-script" }
func (*auditScriptModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

// newAuditVerifier builds the auditor over a scratch workspace. The read round
// answers with no tool call, so the verdict is the model's second response.
func newAuditVerifier(t *testing.T, verdict string, usage ai.Usage) *Verifier {
	t.Helper()

	workspaceRoot, err := workspace.Open(t.TempDir())
	require.NoError(t, err)

	tree, err := workspace.OpenTree(workspaceRoot)
	require.NoError(t, err)

	t.Cleanup(func() { _ = tree.Close() })

	model := &auditScriptModel{rounds: []*ai.Response{
		{Message: ai.AssistantText("no further reads"), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}},
		{Message: ai.AssistantText(verdict), Usage: usage},
	}}

	verifier, err := NewVerifier(t.Context(), model, tree)
	require.NoError(t, err)

	return verifier
}

// auditEvidence is the smallest ledger that passes the auditor's own evidence
// gate, so a test reaches the verdict rather than the evidence problem.
func auditEvidence() Evidence {
	return Evidence{Records: []Record{{ID: "record-1", Tool: "shell", OK: true}}}
}

// TestVerifierJudgesAContractBreakingVerdict pins the auditor's own rule: a
// verdict that parses but breaks its document contract is not a failed run, it
// is an unusable audit whose reason becomes the next gap.
func TestVerifierJudgesAContractBreakingVerdict(t *testing.T) {
	t.Parallel()

	overBoundsGaps := make([]string, MaxGaps+1)
	for index := range overBoundsGaps {
		overBoundsGaps[index] = fmt.Sprintf("gap-%d", index)
	}

	overBoundsReferences := make([]string, MaxRecords+1)
	for index := range overBoundsReferences {
		overBoundsReferences[index] = fmt.Sprintf("reference-%d", index)
	}

	tests := []struct {
		name    string
		verdict Verdict
		problem string
	}{
		{
			name:    "empty reason",
			verdict: Verdict{Gaps: []string{"the recorded read is stale"}},
			problem: "carries no reason",
		},
		{
			name:    "over-long reason",
			verdict: Verdict{Reason: strings.Repeat("x", MaxResultBytes+1)},
			problem: "reason exceeds 4096 bytes",
		},
		{
			name:    "over-bounds gaps",
			verdict: Verdict{Reason: "the migration is unproven", Gaps: overBoundsGaps},
			problem: fmt.Sprintf("carries more than %d gaps", MaxGaps),
		},
		{
			name:    "over-bounds references",
			verdict: Verdict{Reason: "the migration is unproven", References: overBoundsReferences},
			problem: fmt.Sprintf("carries more than %d references", MaxRecords),
		},
		{
			name:    "blank gap",
			verdict: Verdict{Reason: "the migration is unproven", Gaps: []string{"  "}},
			problem: "carries a blank gap",
		},
		{
			name:    "over-long gap",
			verdict: Verdict{Reason: "the migration is unproven", Gaps: []string{strings.Repeat("x", MaxResultBytes+1)}},
			problem: "gap exceeds 4096 bytes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := Encode(test.verdict)
			require.NoError(t, err)

			verifier := newAuditVerifier(t, string(encoded), ai.Usage{InputTokens: 4, OutputTokens: 1})

			verdict, usage, err := verifier.Verify(
				t.Context(), "the migration ran", auditEvidence(), continuation.Limits{}, continuation.Accounting{},
			)
			require.NoError(t, err)
			assert.False(t, verdict.Verified)
			assert.Contains(t, verdict.Reason, test.problem)
			assert.Equal(t, []string{verdict.Reason}, verdict.Gaps)
			assert.Equal(t, ai.Usage{InputTokens: 9, OutputTokens: 2}, usage)
		})
	}
}

// TestVerifierKeepsAnImpossibleUsageAnError pins the other half of the split: an
// impossible token counter is not a document-shape violation, so the audit
// fails the Decision instead of becoming a negative verdict.
func TestVerifierKeepsAnImpossibleUsageAnError(t *testing.T) {
	t.Parallel()

	encoded, err := Encode(Verdict{Reason: "the migration is unproven", Gaps: []string{"run the migration"}})
	require.NoError(t, err)

	verifier := newAuditVerifier(t, string(encoded), ai.Usage{InputTokens: -1 << 20})

	verdict, usage, err := verifier.Verify(
		t.Context(), "the migration ran", auditEvidence(), continuation.Limits{}, continuation.Accounting{},
	)
	require.ErrorIs(t, err, goal.ErrInvalid)
	assert.Empty(t, verdict.Reason)
	assert.Equal(t, ai.Usage{InputTokens: 5 - 1<<20, OutputTokens: 1}, usage)
}

// auditWorkspace opens a scratch workspace for the auditor and returns its root
// path with its tree.
func auditWorkspace(t *testing.T) (string, *workspace.Tree) {
	t.Helper()

	root, err := workspace.Open(t.TempDir())
	require.NoError(t, err)

	tree, err := workspace.OpenTree(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tree.Close() })

	return root.Root(), tree
}

// auditReadEnvelope runs the auditor's own read tool over the tree, so a test can
// cite a captured read with exactly the arguments and digest the auditor sees.
func auditReadEnvelope(t *testing.T, tree *workspace.Tree, path string) (ai.JSON, string) {
	t.Helper()

	local, err := tools.NewCatalog(tree, tools.DefaultLimits())
	require.NoError(t, err)

	policy := catalog.Policy{TenantID: "goal-verifier", Allowlist: []string{readTool}, MaxRisk: catalog.RiskRead}

	readTools, err := local.Tools(t.Context(), policy, readTool)
	require.NoError(t, err)
	require.Len(t, readTools, 1)

	arguments := ai.JSON(fmt.Sprintf(`{"path":%q}`, path))

	content, err := readTools[0].Exec(t.Context(), agent.ToolCall{ID: "audit-read", Name: readTool, Args: arguments})
	require.NoError(t, err)

	return CanonicalArguments(arguments), Digest(PartsText(content))
}

// auditReadRound answers the auditor's read round with one read of path, then
// stops reading, then returns verdict.
func auditReadRound(t *testing.T, path, verdict string) *auditScriptModel {
	t.Helper()

	return &auditScriptModel{rounds: []*ai.Response{
		{
			Message: ai.Assistant(ai.ToolCallPart{ID: "audit-read", Name: readTool, Args: ai.JSON(fmt.Sprintf(`{"path":%q}`, path))}),
			Usage:   ai.Usage{InputTokens: 5, OutputTokens: 1},
		},
		{Message: ai.AssistantText("no further reads"), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}},
		{Message: ai.AssistantText(verdict), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}},
	}}
}

// TestVerifierTreatsAnOverEnvelopeReadAsNotAchieved pins the auditor's read
// envelope against its own catalog: the catalog returns far more than the
// envelope, so an over-envelope read is a read the audit cannot cite rather than
// a failed run.
func TestVerifierTreatsAnOverEnvelopeReadAsNotAchieved(t *testing.T) {
	t.Parallel()

	root, tree := auditWorkspace(t)
	// 300 lines of 100 bytes is inside the read tool's own limits (2,000 lines,
	// 50 KiB) and outside the auditor's envelope.
	content := strings.Repeat(strings.Repeat("x", 99)+"\n", 300)
	require.NoError(t, os.WriteFile(filepath.Join(root, "big.txt"), []byte(content), 0o600))

	verifier, err := NewVerifier(t.Context(), auditReadRound(t, "big.txt", `{"verified":true,"reason":"read it","gaps":[],"references":[]}`), tree)
	require.NoError(t, err)

	verdict, _, err := verifier.Verify(
		t.Context(), "the migration ran", auditEvidence(), continuation.Limits{}, continuation.Accounting{},
	)
	require.NoError(t, err)
	assert.False(t, verdict.Verified)
	assert.Contains(t, verdict.Reason, "evidence envelope")
	assert.Equal(t, []string{verdict.Reason}, verdict.Gaps)
}

// TestVerifierRevalidatesOnlyTheReadsItsVerdictCites pins the narrowed proof
// precondition: the auditor may make at most eight calls, so a ledger with more
// captured read records than that still verifies while the cited record is
// re-read with identical arguments and digest.
func TestVerifierRevalidatesOnlyTheReadsItsVerdictCites(t *testing.T) {
	t.Parallel()

	root, tree := auditWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "proof.txt"), []byte("actual implementation\n"), 0o600))

	arguments, digest := auditReadEnvelope(t, tree, "proof.txt")

	records := []Record{{ID: "record-1", Tool: readTool, Arguments: arguments, Digest: digest, OK: true}}
	for index := 2; index <= 10; index++ {
		records = append(records, Record{
			ID: fmt.Sprintf("record-%d", index), Tool: readTool,
			Arguments: ai.JSON(fmt.Sprintf(`{"path":"stale-%d.txt"}`, index)),
			Digest:    Digest(fmt.Sprintf("stale-%d", index)), OK: true,
		})
	}

	encoded, err := Encode(Verdict{
		Verified: true, Reason: "the cited read matches the workspace",
		Gaps: []string{}, References: []string{"read:audit-read", "record-1"},
	})
	require.NoError(t, err)

	verifier, err := NewVerifier(t.Context(), auditReadRound(t, "proof.txt", string(encoded)), tree)
	require.NoError(t, err)

	verdict, _, err := verifier.Verify(
		t.Context(), "the migration ran", Evidence{Records: records}, continuation.Limits{}, continuation.Accounting{},
	)
	require.NoError(t, err)
	require.True(t, verdict.Verified, verdict.Reason)
}

// TestVerifierRefusesACitedReadThatWasNotReRead pins the half that must still
// bite: a verdict citing a captured file read the auditor never re-read cannot
// verify the Goal.
func TestVerifierRefusesACitedReadThatWasNotReRead(t *testing.T) {
	t.Parallel()

	root, tree := auditWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "proof.txt"), []byte("actual implementation\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "other.txt"), []byte("something else\n"), 0o600))

	arguments, digest := auditReadEnvelope(t, tree, "other.txt")

	encoded, err := Encode(Verdict{
		Verified: true, Reason: "the cited read matches the workspace",
		Gaps: []string{}, References: []string{"read:audit-read", "record-1"},
	})
	require.NoError(t, err)

	verifier, err := NewVerifier(t.Context(), auditReadRound(t, "proof.txt", string(encoded)), tree)
	require.NoError(t, err)

	verdict, _, err := verifier.Verify(
		t.Context(), "the migration ran",
		Evidence{Records: []Record{{ID: "record-1", Tool: readTool, Arguments: arguments, Digest: digest, OK: true}}},
		continuation.Limits{}, continuation.Accounting{},
	)
	require.NoError(t, err)
	assert.False(t, verdict.Verified)
	assert.Contains(t, verdict.Reason, "not revalidated")
}
