package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/rsbin/pips/agent/extension"
	"github.com/rsbin/pips/internal/coding/agentplugin"
	"github.com/rsbin/pips/internal/coding/resource"
	"github.com/rsbin/pips/internal/coding/skillsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type integrationTestLifecycle struct {
	stops       atomic.Int32
	stopErr     error
	onFirstOnly bool
}

func (l *integrationTestLifecycle) Start(context.Context) error { return nil }

func (l *integrationTestLifecycle) Stop(context.Context) error {
	count := l.stops.Add(1)
	if l.onFirstOnly && count != 1 {
		return nil
	}

	return l.stopErr
}

func integrationTestExtension(t *testing.T, id string, lifecycle extension.Lifecycle) extension.Extension {
	t.Helper()

	value, err := extension.NewDefinition(extension.Descriptor{
		ID: id, Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{Lifecycle: lifecycle}, nil
	})
	require.NoError(t, err)

	return value
}

func TestIntegrationGenerationDrainsLeasesBeforeClosing(t *testing.T) {
	t.Parallel()

	lifecycle := &integrationTestLifecycle{}
	extensionValue := integrationTestExtension(t, "leased", lifecycle)
	extensions, err := extension.New(extension.WithExtensions(extensionValue))
	require.NoError(t, err)

	activation, err := extensions.Activate(t.Context(), extensionValue)
	require.NoError(t, err)

	generation := newIntegrationGeneration(
		1, activation, nil, resourceResultForGenerationTest(), skillsettings.Empty(), "", agentplugin.Result{},
	)
	require.NoError(t, generation.acquire())

	require.NoError(t, generation.retire(t.Context()))
	assert.Zero(t, lifecycle.stops.Load(), "retirement must wait for the interaction lease")

	require.NoError(t, generation.release(t.Context()))
	assert.Zero(t, lifecycle.stops.Load(), "the Extension Runtime still owns its current generation")
	require.NoError(t, generation.retire(t.Context()))
	require.NoError(t, extensions.Shutdown(t.Context()))
	assert.Equal(t, int32(1), lifecycle.stops.Load())
	require.NoError(t, generation.retire(t.Context()))
	assert.Equal(t, int32(1), lifecycle.stops.Load(), "close must be idempotent")
}

func TestIntegrationGenerationHandoffPublishesNewPolicy(t *testing.T) {
	t.Parallel()

	lifecycle := &integrationTestLifecycle{}
	extensionValue := integrationTestExtension(t, "handoff", lifecycle)
	extensions, err := extension.New(extension.WithExtensions(extensionValue))
	require.NoError(t, err)
	activation, err := extensions.Activate(t.Context(), extensionValue)
	require.NoError(t, err)

	previous := newIntegrationGeneration(
		1, activation, nil, resourceResultForGenerationTest(), skillsettings.Empty(), "old", agentplugin.Result{},
	)
	nextPolicy := skillsettings.Empty().WithDisabled(
		skillsettings.Ref{Source: "user:pips/SKILL.md", Name: "review"}, true,
	)
	next, err := previous.handoff(2, nextPolicy)
	require.NoError(t, err)
	assert.Nil(t, previous.snapshot())
	assert.Equal(t, "old", next.projectInstructionsSnapshot())
	assert.True(t, next.skillPolicySnapshot().IsDisabled(
		skillsettings.Ref{Source: "user:pips/SKILL.md", Name: "review"},
	))

	require.NoError(t, previous.retire(t.Context()))
	require.NoError(t, next.retire(t.Context()))
	require.NoError(t, extensions.Shutdown(t.Context()))
	assert.Equal(t, int32(1), lifecycle.stops.Load())
}

func TestIntegrationGenerationAdoptsInstalledActivationWithCleanupError(t *testing.T) {
	t.Parallel()

	stopErr := errors.New("retired lifecycle failed")
	oldLifecycle := &integrationTestLifecycle{stopErr: stopErr}
	nextLifecycle := &integrationTestLifecycle{}
	extensions, err := extension.New()
	require.NoError(t, err)

	old, err := extensions.Activate(t.Context(), integrationTestExtension(t, "old", oldLifecycle))
	require.NoError(t, err)
	require.NoError(t, old.Release(t.Context()))

	activation, activationErr := extensions.Activate(
		t.Context(), integrationTestExtension(t, "next", nextLifecycle),
	)
	require.NotNil(t, activation)
	require.ErrorIs(t, activationErr, stopErr)

	generation := newIntegrationGeneration(
		1, activation, nil, resourceResultForGenerationTest(), skillsettings.Empty(), "", agentplugin.Result{},
	)
	// The non-nil activation is the adopted complete generation. The prior
	// cleanup error is reported separately by Activate and does not cause the
	// installed activation to be released as if publication had failed.
	require.NoError(t, generation.retire(t.Context()))
	require.NoError(t, extensions.Shutdown(t.Context()))
	assert.Equal(t, int32(1), nextLifecycle.stops.Load())
}

func TestIntegrationGenerationIncludesAgentPluginSkills(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "skills", "portable"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plugin.json"), []byte(`{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "portable-plugin"
}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "skills", "portable", "SKILL.md"), []byte(`---
name: portable
description: A portable plugin Skill.
---
Instructions.
`), 0o600))
	plugins, err := agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)

	generation := newIntegrationGeneration(
		1, nil, nil, resource.Result{}, skillsettings.Empty(), "", plugins,
	)
	entries := generation.skillEntries(nil)
	require.Len(t, entries, 1)
	assert.Equal(t, "portable", entries[0].Skill.Name)
	assert.Contains(t, entries[0].Skill.Source, "agent-plugin:")
}

func TestRuntimeReloadRejectsCandidateWithoutPublishing(t *testing.T) {
	t.Parallel()

	failPrepare := atomic.Bool{}
	value, err := extension.NewDefinition(extension.Descriptor{
		ID: "reload-failure", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		if failPrepare.Load() {
			return extension.Contribution{}, errors.New("candidate prepare failed")
		}

		return extension.Contribution{}, nil
	})
	require.NoError(t, err)
	runtime := openTestRuntimeWithExtensions(
		t,
		newRuntimeModel(runtimeTextResponse("unused")),
		value,
	)
	before := runtime.integration

	failPrepare.Store(true)

	err = runtime.Reload(t.Context())
	require.Error(t, err)
	_, published := PublishedReloadGeneration(err)
	assert.False(t, published)
	runtime.mu.Lock()
	after := runtime.integration
	runtime.mu.Unlock()
	assert.Same(t, before, after)
	assert.Equal(t, uint64(1), after.ID())
	assert.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeReloadKeepsNewGenerationAfterRetiredCleanupFailure(t *testing.T) {
	t.Parallel()

	stopErr := errors.New("old generation cleanup failed")
	oldLifecycle := &integrationTestLifecycle{stopErr: stopErr, onFirstOnly: true}
	runtime := openTestRuntimeWithExtensions(
		t,
		newRuntimeModel(runtimeTextResponse("unused")),
		integrationTestExtension(t, "reloadable", oldLifecycle),
	)

	err := runtime.Reload(t.Context())
	require.ErrorIs(t, err, stopErr)
	generationID, published := PublishedReloadGeneration(err)
	require.True(t, published)
	assert.Equal(t, uint64(2), generationID)

	runtime.mu.Lock()
	generation := runtime.integration
	runtime.mu.Unlock()
	require.NotNil(t, generation)
	assert.Equal(t, uint64(2), generation.ID())
	assert.Equal(t, int32(1), oldLifecycle.stops.Load())
	assert.NoError(t, runtime.Close(t.Context()))
}

// resource.Result has intentionally no public constructor; keeping this helper
// local makes the generation lifecycle tests independent of resource discovery.
func resourceResultForGenerationTest() (result resource.Result) { return result }
