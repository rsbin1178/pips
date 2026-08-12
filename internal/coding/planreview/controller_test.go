//nolint:wsl_v5 // Protocol fixtures keep each action adjacent to its state assertions.
package planreview_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/plandoc"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRevision = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestControllerPausesExactCurrentRevisionAndContinues(t *testing.T) {
	t.Parallel()

	resolver := &recordingResolver{}
	controller := newController(t, resolver)
	call := ai.ToolCallPart{
		ID: "submit-1", Name: planreview.ToolName,
		Args: ai.JSON(`{"expected_revision":"` + testRevision + `"}`),
	}

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
		ID: call.ID, Name: call.Name, Args: call.Args,
	}})
	require.Equal(t, agent.ToolDecisionPause, decision.Action)
	request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, testRevision, request.Revision)
	assert.EqualValues(t, 128, request.Size)

	require.NoError(t, controller.Resolve(planreview.Resolution{
		RequestID: request.ID,
		Revision:  request.Revision,
		Decision:  planreview.DecisionContinue,
		Feedback:  "Cover rollback behavior.",
	}))
	require.Len(t, resolver.values, 1)
	assert.Contains(t, resultText(resolver.values[0].Content), "Cover rollback behavior.")
	_, accepted := controller.AcceptedRevision()
	assert.False(t, accepted)
}

func TestControllerApprovalFreezesRemainingTools(t *testing.T) {
	t.Parallel()

	resolver := &recordingResolver{}
	controller := newController(t, resolver)
	call := ai.ToolCallPart{
		ID: "submit-approval", Name: planreview.ToolName,
		Args: ai.JSON(`{"expected_revision":"` + testRevision + `"}`),
	}
	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
		ID: call.ID, Name: call.Name, Args: call.Args,
	}})
	require.Equal(t, agent.ToolDecisionPause, decision.Action)
	request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NoError(t, controller.Resolve(planreview.Resolution{
		RequestID: request.ID,
		Revision:  request.Revision,
		Decision:  planreview.DecisionApprove,
	}))

	revision, accepted := controller.AcceptedRevision()
	assert.True(t, accepted)
	assert.Equal(t, testRevision, revision)
	denied := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
		ID: "later", Name: "write_plan", Args: ai.JSON(`{}`),
	}})
	assert.Equal(t, agent.ToolDecisionDeny, denied.Action)
	assert.Contains(t, denied.Reason, "approved")

	controller.ClearAccepted()
	_, accepted = controller.AcceptedRevision()
	assert.False(t, accepted)
}

func TestControllerRejectsStaleAndMalformedSubmissions(t *testing.T) {
	t.Parallel()

	controller := newController(t, &recordingResolver{})
	tests := []string{
		`{}`,
		`{"expected_revision":"bad"}`,
		`{"expected_revision":"` + testRevision + `","path":"plan.md"}`,
		`{"expected_revision":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`,
	}
	for _, arguments := range tests {
		decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
			ID: "invalid", Name: planreview.ToolName, Args: ai.JSON(arguments),
		}})
		assert.Equal(t, agent.ToolDecisionDeny, decision.Action, arguments)
		assert.NotContains(t, decision.Reason, "/", arguments)
	}
}

func TestCatalogAdvertisesAtomicPresentationFields(t *testing.T) {
	t.Parallel()

	controller := newController(t, &recordingResolver{})
	catalogValue, err := controller.Catalog()
	require.NoError(t, err)
	tools, err := catalogValue.Snapshot(
		t.Context(),
		catalog.AllowAll("test", catalog.RiskPrivileged),
	)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	schema := tools[0].Decl().InputSchema
	require.NotNil(t, schema)
	assert.ElementsMatch(t, []string{"expected_revision", "content"}, schema.Required)
	assert.ElementsMatch(t, []string{"expected_revision", "content"}, mapKeys(schema.Properties))
}

func TestControllerPresentsPlanAtomicallyAndReconcilesIdempotently(t *testing.T) {
	t.Parallel()

	resolver := &recordingResolver{}
	repo := &memoryRepository{}
	controller, err := planreview.NewController(
		repo,
		plandoc.Ref{SessionID: "session", WorkspaceID: "workspace"},
		resolver,
	)
	require.NoError(t, err)
	call := ai.ToolCallPart{
		ID: "present-1", Name: planreview.PresentToolName,
		Args: ai.JSON(`{"expected_revision":"","content":"# Plan\n\nBuild it safely."}`),
	}

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall:  agent.ToolCall{ID: call.ID, Name: call.Name, Args: call.Args},
		BatchSize: 1,
	})
	require.Equal(t, agent.ToolDecisionPause, decision.Action)
	assert.Zero(t, repo.replaces, "BeforeTool must not mutate the Plan repository")

	request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, "# Plan\n\nBuild it safely.", request.Content)
	assert.Equal(t, int64(len(request.Content)), request.Size)
	assert.Equal(t, 1, repo.replaces)

	restarted, err := planreview.NewController(
		repo,
		plandoc.Ref{SessionID: "session", WorkspaceID: "workspace"},
		resolver,
	)
	require.NoError(t, err)
	recovered, err := restarted.Reconcile(t.Context(), []ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, recovered)
	assert.Equal(t, *request, *recovered)
	assert.Equal(t, 1, repo.replaces, "recovery must not replace already-applied content")
}

func TestControllerRejectsBatchedPresentationBeforeWrite(t *testing.T) {
	t.Parallel()

	repo := &memoryRepository{}
	controller, err := planreview.NewController(
		repo,
		plandoc.Ref{SessionID: "session", WorkspaceID: "workspace"},
		&recordingResolver{},
	)
	require.NoError(t, err)
	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{
			ID: "present-batched", Name: planreview.PresentToolName,
			Args: ai.JSON(`{"expected_revision":"","content":"# Plan"}`),
		},
		BatchIndex: 0, BatchSize: 2,
	})
	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Zero(t, repo.replaces)
}

type staticRepository struct{ document plandoc.Document }

func (r staticRepository) Read(context.Context, plandoc.Ref) (plandoc.Document, error) {
	return r.document, nil
}

type memoryRepository struct {
	document plandoc.Document
	replaces int
}

func (r *memoryRepository) Read(context.Context, plandoc.Ref) (plandoc.Document, error) {
	if r.document.Revision == "" {
		return plandoc.Document{}, plandoc.ErrNotFound
	}

	return r.document, nil
}

func (r *memoryRepository) Replace(
	_ context.Context,
	ref plandoc.Ref,
	expected string,
	content string,
) (plandoc.Document, error) {
	if expected != r.document.Revision {
		return plandoc.Document{}, plandoc.ErrConflict
	}

	sum := sha256.Sum256([]byte(content))
	r.document = plandoc.Document{
		Ref: ref, Revision: hex.EncodeToString(sum[:]), Content: content, Size: int64(len(content)),
	}
	r.replaces++

	return r.document, nil
}

func (*memoryRepository) Fork(context.Context, plandoc.Ref, plandoc.Ref) error { return nil }

func (staticRepository) Replace(context.Context, plandoc.Ref, string, string) (plandoc.Document, error) {
	return plandoc.Document{}, errors.New("unexpected Replace")
}

func (staticRepository) Fork(context.Context, plandoc.Ref, plandoc.Ref) error {
	return errors.New("unexpected Fork")
}

type recordingResolver struct{ values []agent.ToolResolution }

func (r *recordingResolver) ResolveToolCalls(values ...agent.ToolResolution) error {
	r.values = append(r.values, values...)
	return nil
}

func newController(t *testing.T, resolver *recordingResolver) *planreview.Controller {
	t.Helper()
	controller, err := planreview.NewController(
		staticRepository{document: plandoc.Document{Revision: testRevision, Size: 128}},
		plandoc.Ref{SessionID: "session", WorkspaceID: "workspace"},
		resolver,
	)
	require.NoError(t, err)

	return controller
}

func resultText(parts []ai.Part) string {
	if len(parts) != 1 {
		return ""
	}
	value, _ := parts[0].(ai.TextPart)

	return value.Text
}

func mapKeys(values map[string]*ai.Schema) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}

	return result
}
