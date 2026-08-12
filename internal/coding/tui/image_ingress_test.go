package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/attachment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBridgeImageIngressInsertsProtectedImageAndRearms(t *testing.T) {
	t.Parallel()

	ingress := &imageIngressStub{results: make(chan imageIngressResult, 1)}
	model := readyModel(t, true)
	model.options.ImageIngress = ingress

	image := normalizedComposerImage(t, "clipboard.png", 0)
	ingress.results <- imageIngressResult{image: image}

	command := model.waitBridgeImage()
	require.NotNil(t, command)

	_, next := model.Update(command())
	require.NotNil(t, next)
	assert.Equal(t, 1, ingress.calls)
	assert.Contains(t, model.composer.Value(), "[Image #1 · clipboard.png]")
	require.Len(t, model.composer.elements, 1)
	assert.True(t, image.Equal(model.composer.elements[0].image))
}

func TestBridgeImageIngressErrorDoesNotMutateDraft(t *testing.T) {
	t.Parallel()

	ingress := &imageIngressStub{results: make(chan imageIngressResult, 1)}
	model := readyModel(t, true)
	model.options.ImageIngress = ingress
	model.composer.SetValue("keep exact")
	before := model.composer.Snapshot()

	ingress.results <- imageIngressResult{err: errors.New("bridge stopped")}

	_, next := model.Update(model.waitBridgeImage()())
	require.NotNil(t, next)
	assert.Equal(t, before, model.composer.Snapshot())
	require.ErrorContains(t, model.streamErr, "bridge stopped")
}

type imageIngressResult struct {
	image attachment.Image
	err   error
}

type imageIngressStub struct {
	results chan imageIngressResult
	calls   int
}

func (s *imageIngressStub) Receive(ctx context.Context) (attachment.Image, error) {
	s.calls++

	select {
	case result := <-s.results:
		return result.image, result.err
	case <-ctx.Done():
		return attachment.Image{}, ctx.Err()
	}
}
