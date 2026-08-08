package acp

import (
	"encoding/json"
	"testing"

	"github.com/rsbin/pips/internal/coding/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuestionElicitationRoundTrip(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("request-1", "call-1", question.Spec{Questions: []question.Question{
		{
			Header: "Scope", Question: "Which scope?", Options: []question.Option{
				{Label: "Package", Description: "Only this package"},
				{Label: "Repo", Description: "The whole repository"},
			},
		},
		{
			Header: "Checks", Question: "Which checks?", Multiple: true, Options: []question.Option{
				{Label: "Tests", Description: "Run tests"},
				{Label: "Lint", Description: "Run lint"},
			},
		},
	}})
	require.NoError(t, err)

	wire, err := elicitationForQuestion("session-1", request)
	require.NoError(t, err)
	assert.Equal(t, "session-1", string(wire.SessionID))
	assert.Equal(t, "call-1", string(*wire.ToolCallID))
	assert.Equal(t, "array", wire.RequestedSchema.Properties["question_2"].Type)

	resolution, accepted, err := resolutionFromElicitation(request, createElicitationResponse{
		Action: "accept",
		Content: map[string]json.RawMessage{
			"question_1": json.RawMessage(`"Package"`),
			"question_2": json.RawMessage(`["Tests","Lint"]`),
		},
	})
	require.NoError(t, err)
	assert.True(t, accepted)
	assert.Equal(t, []string{"Package"}, resolution.Answers[0].Selections)
	assert.Equal(t, []string{"Tests", "Lint"}, resolution.Answers[1].Selections)
}

func TestQuestionElicitationRejectsSchemaMismatch(t *testing.T) {
	t.Parallel()

	request, err := question.NewFreeformRequest("request-1", "call-1", "Explain")
	require.NoError(t, err)
	_, _, err = resolutionFromElicitation(request, createElicitationResponse{
		Action: "accept", Content: map[string]json.RawMessage{"response": json.RawMessage(`42`)},
	})
	require.ErrorIs(t, err, ErrInvalid)
}
