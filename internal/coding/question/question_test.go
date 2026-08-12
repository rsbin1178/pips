package question_test

import (
	"testing"

	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestAndResolution(t *testing.T) {
	t.Parallel()

	spec := testSpec()
	request, err := question.NewRequest("request-1", "call-1", spec)
	require.NoError(t, err)
	require.NoError(t, question.ValidateRequest(request))

	resolution := question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{
			{Selections: []string{"React"}},
			{Selections: []string{"Core", "Tests"}},
		},
	}
	require.NoError(t, question.ValidateResolution(request, resolution))

	cloned := question.CloneRequest(request)
	cloned.Questions[0].Options[0].Label = "changed"
	assert.Equal(t, "React", request.Questions[0].Options[0].Label)
}

func TestResolutionAllowsCustomOrChat(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("request-1", "call-1", testSpec())
	require.NoError(t, err)

	require.NoError(t, question.ValidateResolution(request, question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{
			{Custom: "Solid"},
			{Selections: []string{"Tests"}},
		},
	}))
	require.NoError(t, question.ValidateResolution(request, question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Chat: "Can we compare the tradeoffs first?",
	}))
}

func TestResolutionRejectsStaleOrAmbiguousAnswers(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("request-1", "call-1", testSpec())
	require.NoError(t, err)

	tests := []question.Resolution{
		{RequestID: "stale", SchemaDigest: request.SchemaDigest, Chat: "discuss"},
		{RequestID: request.ID, SchemaDigest: request.SchemaDigest},
		{
			RequestID: request.ID, SchemaDigest: request.SchemaDigest,
			Answers: []question.Answer{
				{Selections: []string{"React", "Vue"}},
				{Selections: []string{"Tests"}},
			},
		},
		{
			RequestID: request.ID, SchemaDigest: request.SchemaDigest,
			Answers: []question.Answer{
				{Selections: []string{"React"}},
				{Selections: []string{"Tests", "Core"}},
			},
		},
		{
			RequestID: request.ID, SchemaDigest: request.SchemaDigest,
			Answers: []question.Answer{
				{Selections: []string{"React"}},
				{Selections: []string{"Tests"}},
			},
			Chat: "both",
		},
	}

	for _, resolution := range tests {
		require.ErrorIs(t, question.ValidateResolution(request, resolution), question.ErrInvalid)
	}
}

func TestValidateSpecRejectsMalformedSchemas(t *testing.T) {
	t.Parallel()

	tests := []question.Spec{
		{},
		{Questions: []question.Question{{Header: "H", Question: "Q"}}},
		{Questions: []question.Question{{
			Header: "H", Question: "Q",
			Options: []question.Option{
				{Label: "same", Description: "one"},
				{Label: "same", Description: "two"},
			},
		}}},
	}
	for _, spec := range tests {
		require.ErrorIs(t, question.ValidateSpec(spec), question.ErrInvalid)
	}
}

func testSpec() question.Spec {
	return question.Spec{Questions: []question.Question{
		{
			Header: "Framework", Question: "Which framework?",
			Options: []question.Option{
				{Label: "React", Description: "Established ecosystem", Preview: "**Recommended**"},
				{Label: "Vue", Description: "Progressive framework"},
			},
		},
		{
			Header: "Scope", Question: "Which areas?", Multiple: true,
			Options: []question.Option{
				{Label: "Core", Description: "Core implementation"},
				{Label: "Tests", Description: "Test coverage"},
			},
		},
	}}
}
