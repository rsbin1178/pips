package ai_test

import (
	"context"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taggingMiddleware records the order middleware layers run in.
func taggingMiddleware(tag string, order *[]string) ai.Middleware {
	return func(next ai.LanguageModel) ai.LanguageModel {
		return &taggedModel{next: next, tag: tag, order: order}
	}
}

type taggedModel struct {
	next  ai.LanguageModel
	tag   string
	order *[]string
}

func (m *taggedModel) Generate(ctx context.Context, req ai.Request) (*ai.Response, error) {
	*m.order = append(*m.order, m.tag)
	return m.next.Generate(ctx, req)
}

func (m *taggedModel) Stream(ctx context.Context, req ai.Request) ai.Stream {
	*m.order = append(*m.order, m.tag)
	return m.next.Stream(ctx, req)
}

func (m *taggedModel) Provider() ai.Provider         { return m.next.Provider() }
func (m *taggedModel) ModelID() string               { return m.next.ModelID() }
func (m *taggedModel) Capabilities() ai.Capabilities { return m.next.Capabilities() }

func TestChainOrder(t *testing.T) {
	t.Parallel()

	var order []string

	base := &staticModel{resp: &ai.Response{Message: ai.AssistantText("ok")}}

	model := ai.Chain(base,
		taggingMiddleware("outer", &order),
		taggingMiddleware("inner", &order),
	)

	_, err := model.Generate(t.Context(), ai.Request{})
	require.NoError(t, err)
	assert.Equal(t, []string{"outer", "inner"}, order)

	// Identity passthrough still works.
	assert.Equal(t, ai.Provider("static"), model.Provider())
	assert.Equal(t, "static-1", model.ModelID())
}

func TestChainEmpty(t *testing.T) {
	t.Parallel()

	base := &staticModel{resp: &ai.Response{Message: ai.AssistantText("ok")}}
	assert.Equal(t, ai.LanguageModel(base), ai.Chain(base))
}
