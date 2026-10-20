package question_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/question"
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

func TestControllerAcceptsJSONEncodedQuestionsCompatibility(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)

	questions, err := json.Marshal(testSpec().Questions)
	require.NoError(t, err)
	args, err := json.Marshal(struct {
		Questions string `json:"questions"`
	}{Questions: string(questions)})
	require.NoError(t, err)

	call := ai.ToolCallPart{
		ID: "stringified-questions", Name: question.ToolName, Args: args,
	}
	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
		ID: call.ID, Name: call.Name, Args: call.Args,
	}})
	assert.Equal(t, agent.ToolDecisionPause, decision.Action)

	request, err := controller.Reconcile([]ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, testSpec().Questions, request.Questions)
}

func TestControllerExplainsMalformedJSONEncodedQuestions(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)
	args, err := json.Marshal(struct {
		Questions string `json:"questions"`
	}{Questions: `[{"header":`})
	require.NoError(t, err)

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
		ID: "malformed-stringified-questions", Name: question.ToolName, Args: args,
	}})

	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Contains(t, decision.Reason, "decode JSON-encoded questions")
}

func TestControllerKeepsJSONEncodedQuestionsStrict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		questions string
		want      string
	}{
		{
			name: "unknown nested field",
			questions: `[{"header":"H","question":"Q","options":[` +
				`{"label":"A","description":"A","unknown":true},` +
				`{"label":"B","description":"B"}]}]`,
			want: "unknown field",
		},
		{
			name: "duplicate nested field",
			questions: `[{"header":"H","header":"H2","question":"Q","options":[` +
				`{"label":"A","description":"A"},` +
				`{"label":"B","description":"B"}]}]`,
			want: "duplicate object key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			controller, err := question.NewController(&recordingResolver{})
			require.NoError(t, err)
			args, err := json.Marshal(struct {
				Questions string `json:"questions"`
			}{Questions: test.questions})
			require.NoError(t, err)

			decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
				ID: "strict-stringified-questions", Name: question.ToolName, Args: args,
			}})
			assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
			assert.Contains(t, decision.Reason, test.want)
		})
	}
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
	assert.Equal(t, question.RejectionToolResult, rejectedText.Text)
	require.ErrorIs(t, controller.Reject(request.ID, request.SchemaDigest), question.ErrNoPending)
}

func TestControllerExplainsInvalidChoiceCount(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)

	spec := testSpec()
	spec.Questions[0].Options = append(
		spec.Questions[0].Options,
		question.Option{Label: "Svelte", Description: "Compiler-first framework"},
		question.Option{Label: "Solid", Description: "Fine-grained reactivity"},
		question.Option{Label: "Other", Description: "Another framework"},
	)
	args, err := json.Marshal(spec)
	require.NoError(t, err)

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
		ID: "too-many-options", Name: question.ToolName, Args: args,
	}})

	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Contains(t, decision.Reason, "two to four options")
}

func TestControllerBoundsInvalidArgumentFeedback(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{
		ID: "oversize-error", Name: question.ToolName,
		Args: ai.JSON(`{"` + strings.Repeat("x", 2048) + `":true}`),
	}})

	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.LessOrEqual(t, len(decision.Reason), 1024)
	assert.True(t, utf8.ValidString(decision.Reason))
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

	tools, err := catalogValue.Snapshot(t.Context(), catalogPolicy())
	require.NoError(t, err)
	require.Len(t, tools, 1)
	declaration := tools[0].Decl()
	assert.Contains(t, declaration.Description, "single ask_user call")
	assert.Contains(t, declaration.Description, "never combine it with other tools")
	assert.Contains(t, declaration.Description, "Prefer structured choices")
	assert.Contains(t, declaration.Description, "assistant text")

	schema := declaration.InputSchema
	require.NotNil(t, schema)
	assert.Empty(t, schema.Required)
	assert.ElementsMatch(t, []string{"questions", "question"}, mapKeys(schema.Properties))
	assert.JSONEq(t, "1", string(schema.Extra["minProperties"]))
	assert.JSONEq(t, "1", string(schema.Extra["maxProperties"]))

	questions := schema.Properties["questions"]
	require.NotNil(t, questions)
	assert.JSONEq(t, "1", string(questions.Extra["minItems"]))
	assert.JSONEq(t, "4", string(questions.Extra["maxItems"]))
	require.NotNil(t, questions.Items)
	assert.ElementsMatch(t, []string{"header", "question"}, questions.Items.Required)
	assert.Contains(t, mapKeys(questions.Items.Properties), "kind")
	assert.Contains(t, mapKeys(questions.Items.Properties), "options")
	assert.NotContains(t, questions.Items.Required, "multiple")
	options := questions.Items.Properties["options"]
	require.NotNil(t, options)
	assert.Nil(t, options.Extra["minItems"])
	assert.JSONEq(t, "4", string(options.Extra["maxItems"]))
	require.NotNil(t, options.Items)
	assert.ElementsMatch(t, []string{"label", "description"}, options.Items.Required)
	assert.NotContains(t, options.Items.Required, "preview")

	kind := questions.Items.Properties["kind"]
	require.NotNil(t, kind)
	assert.ElementsMatch(t, []any{"choice", "text"}, kind.Enum)

	freeform := schema.Properties["question"]
	require.NotNil(t, freeform)
	assert.Equal(t, "string", freeform.Type)

	wireSchema, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.Contains(t, string(wireSchema), `"minItems":1`)
	assert.Contains(t, string(wireSchema), `"maxItems":4`)
	assert.Contains(t, string(wireSchema), `"minProperties":1`)
	assert.Contains(t, string(wireSchema), `"maxProperties":1`)
	assert.Contains(t, string(wireSchema), `"kind"`)
}

func TestControllerPausesFlatAndGenericFreeformFallback(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)

	valid := ai.ToolCallPart{
		ID: "flat-valid", Name: question.TextToolName,
		Args: ai.JSON(`{"question":"Which authentication policy should the system use?"}`),
	}
	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: valid.ID, Name: valid.Name, Args: valid.Args}, BatchSize: 1,
	})
	require.Equal(t, agent.ToolDecisionPause, decision.Action)

	request, err := controller.Reconcile([]ai.ToolCallPart{valid})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, question.RequestFreeform, request.Kind)
	assert.Contains(t, request.Prompt, "authentication policy")

	controller, err = question.NewController(&recordingResolver{})
	require.NoError(t, err)

	malformed := ai.ToolCallPart{
		ID: "flat-malformed", Name: question.TextToolName,
		Args: ai.JSON(`{"question":}`),
	}
	decision = controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: malformed.ID, Name: malformed.Name, Args: malformed.Args}, BatchSize: 1,
	})
	require.Equal(t, agent.ToolDecisionPause, decision.Action)

	request, err = controller.Reconcile([]ai.ToolCallPart{malformed})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, question.RequestFreeform, request.Kind)
	assert.Contains(t, request.Prompt, "could not form a valid structured question")
	assert.NotContains(t, request.Prompt, `{"question":}`)
}

func TestControllerRejectsBatchedQuestionBeforePause(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)
	args, err := json.Marshal(testSpec())
	require.NoError(t, err)

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall:   agent.ToolCall{ID: "batched", Name: question.ToolName, Args: args},
		BatchIndex: 0, BatchSize: 2,
	})
	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Contains(t, decision.Reason, "alone")

	request, err := controller.Reconcile(nil)
	require.NoError(t, err)
	assert.Nil(t, request)
}

func TestControllerAcceptsUnifiedFreeformQuestion(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)

	call := ai.ToolCallPart{
		ID: "freeform", Name: question.ToolName,
		Args: ai.JSON(`{"question":"Which authentication policy should the system use?"}`),
	}
	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: call.ID, Name: call.Name, Args: call.Args}, BatchSize: 1,
	})
	require.Equal(t, agent.ToolDecisionPause, decision.Action)

	request, err := controller.Reconcile([]ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, question.RequestFreeform, request.Kind)
	assert.Contains(t, request.Prompt, "authentication policy")
	assert.Zero(t, request.QueueIndex)
	assert.Zero(t, request.QueueSize)
}

func TestControllerRejectsBothQuestionShapes(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{
			ID: "ambiguous", Name: question.ToolName,
			Args: ai.JSON(`{"questions":[{"header":"H","question":"Q","options":[` +
				`{"label":"A","description":"A"},{"label":"B","description":"B"}]}],` +
				`"question":"also free-form"}`),
		},
		BatchSize: 1,
	})

	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Contains(t, decision.Reason, "exactly one of questions or question")
}

func TestControllerAcceptsMixedChoiceAndTextQuestions(t *testing.T) {
	t.Parallel()

	resolver := &recordingResolver{}
	controller, err := question.NewController(resolver)
	require.NoError(t, err)

	call := ai.ToolCallPart{
		ID: "mixed", Name: question.ToolName,
		Args: ai.JSON(`{"questions":[` +
			`{"header":"Framework","question":"Which framework?","kind":"choice","options":[` +
			`{"label":"React","description":"Components"},{"label":"Vue","description":"Progressive"}]},` +
			`{"header":"Risk","question":"Which risk matters first?","kind":"text"}` +
			`]}`),
	}
	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: call.ID, Name: call.Name, Args: call.Args}, BatchSize: 1,
	})
	require.Equal(t, agent.ToolDecisionPause, decision.Action)

	request, err := controller.Reconcile([]ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, question.KindChoice, request.Questions[0].Kind)
	assert.Equal(t, question.KindText, request.Questions[1].Kind)

	require.NoError(t, controller.Resolve(question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{
			{Selections: []string{"React"}},
			{Custom: "Payment retries"},
		},
	}))
	require.Len(t, resolver.resolutions, 1)
	resolvedText, ok := resolver.resolutions[0].Content[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, resolvedText.Text, "Payment retries")
	assert.Contains(t, resolvedText.Text, "React")
}

func TestControllerQueuesPureAskBatch(t *testing.T) {
	t.Parallel()

	resolver := &recordingResolver{}
	controller, err := question.NewController(resolver)
	require.NoError(t, err)
	args, err := json.Marshal(testSpec())
	require.NoError(t, err)

	first := ai.ToolCallPart{ID: "ask-1", Name: question.ToolName, Args: args}
	second := ai.ToolCallPart{ID: "ask-2", Name: question.ToolName, Args: args}
	batch := []agent.ToolCall{
		{ID: first.ID, Name: first.Name, Args: first.Args},
		{ID: second.ID, Name: second.Name, Args: second.Args},
	}

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: batch[0], BatchIndex: 0, BatchSize: 2, BatchCalls: batch,
	})
	require.Equal(t, agent.ToolDecisionPause, decision.Action)

	request, err := controller.Reconcile([]ai.ToolCallPart{first, second})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, 1, request.QueueIndex)
	assert.Equal(t, 2, request.QueueSize)

	require.NoError(t, controller.Reject(request.ID, request.SchemaDigest))
	require.Len(t, resolver.resolutions, 1)
	assert.Equal(t, first.ID, resolver.resolutions[0].ToolCallID)
	assert.True(t, resolver.resolutions[0].IsError)

	next, err := controller.Reconcile([]ai.ToolCallPart{second})
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, 2, next.QueueIndex)
	assert.Equal(t, 2, next.QueueSize)

	require.NoError(t, controller.Resolve(question.Resolution{
		RequestID: next.ID, SchemaDigest: next.SchemaDigest,
		Answers: []question.Answer{
			{Selections: []string{"React"}},
			{Selections: []string{"Core", "Tests"}},
		},
	}))
	require.Len(t, resolver.resolutions, 2)
	assert.Equal(t, second.ID, resolver.resolutions[1].ToolCallID)
	assert.False(t, resolver.resolutions[1].IsError)

	request, err = controller.Reconcile(nil)
	require.NoError(t, err)
	assert.Nil(t, request)
}

func TestControllerRejectsMixedBatchForEveryAskCall(t *testing.T) {
	t.Parallel()

	controller, err := question.NewController(&recordingResolver{})
	require.NoError(t, err)
	args, err := json.Marshal(testSpec())
	require.NoError(t, err)

	batch := []agent.ToolCall{
		{ID: "ask-1", Name: question.ToolName, Args: args},
		{ID: "read-1", Name: "read", Args: ai.JSON(`{"path":"a.go"}`)},
		{ID: "ask-2", Name: question.ToolName, Args: args},
	}

	decision := controller.BeforeBatch(t.Context(), agent.ToolCallInfo{
		ToolCall: batch[0], BatchIndex: 0, BatchSize: 3, BatchCalls: batch,
	})

	assert.Equal(t, agent.ToolDecisionDenyBatch, decision.Action)
	assert.Contains(t, decision.Reason, question.MixedBatchErrorPrefix)
	assert.Contains(t, decision.Reason, "2 ask calls, 1 other tool")
	assert.Contains(t, decision.Reason, "Send one ask_user call alone")

	// The first batch call may be the non-ask tool; the batch rejection must
	// still arrive before another gate can pause the batch.
	decision = controller.BeforeBatch(t.Context(), agent.ToolCallInfo{
		ToolCall: batch[1], BatchIndex: 1, BatchSize: 3, BatchCalls: batch,
	})
	assert.Equal(t, agent.ToolDecisionDenyBatch, decision.Action)

	// BeforeTool keeps the same refusal when it is wired without BeforeBatch.
	decision = controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: batch[0], BatchIndex: 0, BatchSize: 3, BatchCalls: batch,
	})
	assert.Equal(t, agent.ToolDecisionDenyBatch, decision.Action)
}

func mapKeys(values map[string]*ai.Schema) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}

	return result
}

func catalogPolicy() catalog.Policy {
	return catalog.AllowAll("test", catalog.RiskRead)
}
