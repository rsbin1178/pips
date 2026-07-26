package question_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingResolver struct {
	resolutions []agent.ToolResolution
	err         error
}

func (r *recordingResolver) ResolveToolCalls(values ...agent.ToolResolution) error {
	if r.err != nil {
		return r.err
	}

	r.resolutions = append(r.resolutions, values...)

	return nil
}

func TestControllerPausesResolvesAndReconciles(t *testing.T) {
	t.Parallel()

	resolver := &recordingResolver{}
	controller, err := question.NewController(resolver)
	require.NoError(t, err)
	args, err := json.Marshal(testSpec())
	require.NoError(t, err)

	call := ai.ToolCallPart{ID: "call-1", Name: question.ToolName, Args: args}

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
		ID: call.ID, Name: call.Name, Args: call.Args,
	}})
	assert.Equal(t, agent.ToolDecisionPause, decision.Action)

	request, err := controller.Reconcile([]ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)
	require.NoError(t, controller.Resolve(question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{
			{Selections: []string{"React"}},
			{Selections: []string{"Core", "Tests"}},
		},
	}))
	require.Len(t, resolver.resolutions, 1)
	assert.Equal(t, call.ID, resolver.resolutions[0].ToolCallID)
	assert.False(t, resolver.resolutions[0].IsError)
	resolvedText, ok := resolver.resolutions[0].Content[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, resolvedText.Text, "React")

	request, err = controller.Reconcile(nil)
	require.NoError(t, err)
	assert.Nil(t, request)
}

func TestControllerRejectsMalformedStaleAndCanceledInput(t *testing.T) {
	t.Parallel()

	resolver := &recordingResolver{}
	controller, err := question.NewController(resolver)
	require.NoError(t, err)

	decision := controller.BeforeTool(context.Background(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
		ID: "bad", Name: question.ToolName, Args: []byte(`{"questions":[],"unknown":true}`),
	}})
	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)

	args, err := json.Marshal(testSpec())
	require.NoError(t, err)

	call := ai.ToolCallPart{ID: "call-2", Name: question.ToolName, Args: args}
	request, err := controller.Reconcile([]ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)

	require.ErrorIs(t, controller.Reject("stale", request.SchemaDigest), question.ErrMismatch)
	require.NoError(t, controller.Reject(request.ID, request.SchemaDigest))
	require.Len(t, resolver.resolutions, 1)
	assert.True(t, resolver.resolutions[0].IsError)
	rejectedText, ok := resolver.resolutions[0].Content[0].(ai.TextPart)
	require.True(t, ok)
	assert.NotContains(t, rejectedText.Text, "React")
	require.ErrorIs(t, controller.Reject(request.ID, request.SchemaDigest), question.ErrNoPending)
}

func TestQuestionCatalogHasExactProvenanceAndSchema(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)
	catalogValue, err := controller.Catalog()
	require.NoError(t, err)
	descriptors, err := catalogValue.Search(t.Context(), catalogPolicy(), "")
	require.NoError(t, err)
	require.Len(t, descriptors, 1)
	assert.Equal(t, question.ToolName, descriptors[0].Name)
	assert.Equal(t, question.CatalogID, descriptors[0].Source.ID)
}

func catalogPolicy() catalog.Policy {
	return catalog.AllowAll("test", catalog.RiskRead)
}
