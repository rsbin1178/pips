package bundle_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/bundle"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/extension"
)

func TestBundleActivateComposesRegisteredAndDeclarativeExtensions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	compiled, err := extension.NewDefinition(extension.Descriptor{
		ID:      "compiled",
		Version: "2.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{Tools: []extension.Tool{{
			Value: agent.NewTool("lookup", "Look up a value", func(context.Context, struct{}) (string, error) {
				return "value", nil
			}),
			Risk: catalog.RiskRead,
		}}}, nil
	})
	if err != nil {
		t.Fatalf("new compiled extension: %v", err)
	}

	runtime, err := extension.New(extension.WithExtensions(compiled))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	loader := newLoader(t, completeFS())

	bundle, err := loader.Load(ctx, "packages/demo/pips-bundle.json", bundle.Settings{
		Scope: bundle.ScopeProject,
		Trust: bundle.TrustApproved,
	})
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}

	activation, err := bundle.Activate(ctx, runtime)
	if err != nil {
		t.Fatalf("activate bundle: %v", err)
	}

	t.Cleanup(func() {
		_ = activation.Release(context.Background())
		_ = runtime.Shutdown(context.Background())
	})

	snapshot := activation.Snapshot()
	descriptors := snapshot.Descriptors()

	gotIDs := make([]string, len(descriptors))
	for i, descriptor := range descriptors {
		gotIDs[i] = descriptor.ID
	}

	if want := []string{"compiled", "bundle:demo"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("descriptor IDs = %v, want %v", gotIDs, want)
	}

	if len(snapshot.Skills()) != 1 || len(snapshot.Prompts()) != 1 || len(snapshot.Assets()) != 1 {
		t.Fatalf("snapshot resources: skills=%d prompts=%d assets=%d", len(snapshot.Skills()), len(snapshot.Prompts()), len(snapshot.Assets()))
	}

	if got := snapshot.Diagnostics(); len(got) != 1 || got[0].Capability != "application.optional" {
		t.Fatalf("snapshot diagnostics = %#v", got)
	}

	tools, err := snapshot.Catalog().Snapshot(ctx, catalog.AllowAll("test", catalog.RiskRead))
	if err != nil {
		t.Fatalf("catalog snapshot: %v", err)
	}

	if len(tools) != 1 || tools[0].Decl().Name != "lookup" {
		t.Fatalf("tools = %#v", tools)
	}

	if _, err := snapshot.AgentOptions(ctx, catalog.AllowAll("test", catalog.RiskRead)); err != nil {
		t.Fatalf("agent options: %v", err)
	}

	if _, err := snapshot.HarnessOptions(ctx, catalog.AllowAll("test", catalog.RiskRead)); err != nil {
		t.Fatalf("harness options: %v", err)
	}
}

func TestBundleActivationCapabilityFailureLeavesPriorGeneration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	runtime, err := extension.New()
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	loader := newLoader(t, fstest.MapFS{
		"good.json": &fstest.MapFile{Data: []byte(`{
			"schema":"pips.bundle/v1alpha1","id":"good","version":"1"
		}`)},
		"bad.json": &fstest.MapFile{Data: []byte(`{
			"schema":"pips.bundle/v1alpha1","id":"bad","version":"1",
			"requires":["application.missing"]
		}`)},
	})

	good, err := loader.Load(ctx, "good.json", bundle.Settings{Scope: bundle.ScopeUser})
	if err != nil {
		t.Fatalf("load good: %v", err)
	}

	bad, err := loader.Load(ctx, "bad.json", bundle.Settings{Scope: bundle.ScopeUser})
	if err != nil {
		t.Fatalf("load bad: %v", err)
	}

	goodActivation, err := good.Activate(ctx, runtime)
	if err != nil {
		t.Fatalf("activate good: %v", err)
	}

	goodGeneration := goodActivation.Snapshot().Generation()

	if _, err := bad.Activate(ctx, runtime); !errors.Is(err, extension.ErrCapabilityUnavailable) {
		t.Fatalf("bad activation error = %v", err)
	}

	current, err := runtime.Acquire()
	if err != nil {
		t.Fatalf("acquire current: %v", err)
	}

	if got := current.Snapshot().Generation(); got != goodGeneration {
		t.Fatalf("current generation = %d, want %d", got, goodGeneration)
	}

	if err := current.Release(ctx); err != nil {
		t.Fatalf("release current: %v", err)
	}

	if err := goodActivation.Release(ctx); err != nil {
		t.Fatalf("release good: %v", err)
	}

	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestActivateRejectsDuplicateBundleAndExtensionSelections(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	compiled, err := extension.NewDefinition(extension.Descriptor{ID: "shared", Version: "1"}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{}, nil
	})
	if err != nil {
		t.Fatalf("new extension: %v", err)
	}

	runtime, err := extension.New(extension.WithExtensions(compiled))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	loader := newLoader(t, fstest.MapFS{
		"one.json": &fstest.MapFile{Data: []byte(`{
			"schema":"pips.bundle/v1alpha1","id":"one","version":"1","extensions":["shared"]
		}`)},
		"two.json": &fstest.MapFile{Data: []byte(`{
			"schema":"pips.bundle/v1alpha1","id":"two","version":"1","extensions":["shared"]
		}`)},
	})

	one, err := loader.Load(ctx, "one.json", bundle.Settings{Scope: bundle.ScopeUser})
	if err != nil {
		t.Fatalf("load one: %v", err)
	}

	two, err := loader.Load(ctx, "two.json", bundle.Settings{Scope: bundle.ScopeUser})
	if err != nil {
		t.Fatalf("load two: %v", err)
	}

	if _, err := bundle.Activate(ctx, runtime, one, one); !errors.Is(err, bundle.ErrDuplicate) {
		t.Fatalf("duplicate bundle error = %v", err)
	}

	if _, err := bundle.Activate(ctx, runtime, one, two); !errors.Is(err, bundle.ErrDuplicate) {
		t.Fatalf("duplicate extension error = %v", err)
	}

	if _, err := bundle.Activate(ctx, runtime); !errors.Is(err, bundle.ErrInvalid) {
		t.Fatalf("empty activation error = %v", err)
	}
}

func TestActivateRejectsMissingRegisteredExtension(t *testing.T) {
	t.Parallel()

	runtime, err := extension.New()
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	loader := newLoader(t, fstest.MapFS{
		bundle.DefaultManifestPath: &fstest.MapFile{Data: []byte(`{
			"schema":"pips.bundle/v1alpha1","id":"missing","version":"1","extensions":["not-registered"]
		}`)},
	})

	bundle, err := loader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{Scope: bundle.ScopeUser})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if _, err := bundle.Activate(context.Background(), runtime); !errors.Is(err, extension.ErrNotRegistered) {
		t.Fatalf("activation error = %v", err)
	}
}

func TestActivateDoesNotSelectUnreferencedRegisteredExtensions(t *testing.T) {
	t.Parallel()

	unreferenced, err := extension.NewDefinition(extension.Descriptor{
		ID: "unreferenced", Version: "1",
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{}, nil
	})
	if err != nil {
		t.Fatalf("new extension: %v", err)
	}

	runtime, err := extension.New(extension.WithExtensions(unreferenced))
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	loader := newLoader(t, fstest.MapFS{
		bundle.DefaultManifestPath: &fstest.MapFile{Data: []byte(`{
			"schema":"pips.bundle/v1alpha1","id":"empty","version":"1"
		}`)},
	})

	bundle, err := loader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{Scope: bundle.ScopeUser})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	activation, err := bundle.Activate(context.Background(), runtime)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	descriptors := activation.Snapshot().Descriptors()
	if len(descriptors) != 1 || descriptors[0].ID != "bundle:empty" {
		t.Fatalf("descriptors = %#v", descriptors)
	}

	if err := activation.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}

	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
