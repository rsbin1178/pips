package bundle_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rsbin1178/pips/agent/bundle"
)

func TestLoaderLoadsResourcesRelativeToManifest(t *testing.T) {
	t.Parallel()

	loader := newLoader(t, completeFS())

	bundle, err := loader.Load(context.Background(), "packages/demo/pips-bundle.json", bundle.Settings{
		Scope: bundle.ScopeProject,
		Trust: bundle.TrustApproved,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	manifest := bundle.Manifest()
	if manifest.ID != "demo" || manifest.Version != "1.2.3" {
		t.Fatalf("manifest = %#v", manifest)
	}

	if got, want := bundle.ExtensionIDs(), []string{"compiled"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extension IDs = %v, want %v", got, want)
	}

	if got := bundle.Skills(); len(got) != 1 || got[0].Name != "review" || got[0].Source != "skills/review/SKILL.md" {
		t.Fatalf("skills = %#v", got)
	}

	if got := bundle.Prompts(); len(got) != 1 || got[0].Name != "summarize" || got[0].Content != "Summarize $1." {
		t.Fatalf("prompts = %#v", got)
	}

	if got := bundle.Assets(); len(got) != 1 || got[0].Source != "assets/theme.json" || string(got[0].Data) != `{"color":"blue"}` {
		t.Fatalf("assets = %#v", got)
	}

	manifest.Extensions[0] = "mutated"
	skills := bundle.Skills()
	skills[0].Metadata["owner"] = "mutated"
	assets := bundle.Assets()
	assets[0].Data[0] = 'x'

	if bundle.Manifest().Extensions[0] != "compiled" {
		t.Fatal("manifest extension IDs were mutated")
	}

	if bundle.Skills()[0].Metadata["owner"] != "core" {
		t.Fatal("skill metadata was mutated")
	}

	if string(bundle.Assets()[0].Data) != `{"color":"blue"}` {
		t.Fatal("asset data was mutated")
	}
}

func TestLoaderAppliesFiltersBeforeOpeningResources(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		bundle.DefaultManifestPath: &fstest.MapFile{Data: []byte(`{
			"schema":"pips.bundle/v1alpha1",
			"id":"filtered",
			"version":"1.0.0",
			"extensions":["one","two"],
			"skills":["missing/skill"],
			"prompts":["missing/prompt"],
			"assets":[{"kind":"theme","name":"default","path":"missing/asset"}]
		}`)},
	}
	loader := newLoader(t, fsys)

	bundle, err := loader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{
		Scope: bundle.ScopeUser,
		Filters: bundle.Filters{
			Extensions: bundle.ComponentFilter{Include: []string{"two"}},
			Skills:     bundle.ComponentFilter{Include: []string{}},
			Prompts:    bundle.ComponentFilter{Include: []string{}},
			Assets:     bundle.ComponentFilter{Include: []string{}},
		},
	})
	if err != nil {
		t.Fatalf("load filtered bundle: %v", err)
	}

	if got, want := bundle.ExtensionIDs(), []string{"two"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extension IDs = %v, want %v", got, want)
	}

	if len(bundle.Skills()) != 0 || len(bundle.Prompts()) != 0 || len(bundle.Assets()) != 0 {
		t.Fatal("excluded resources were loaded")
	}

	if got := bundle.Diagnostics(); len(got) != 4 {
		t.Fatalf("filter diagnostics = %#v, want 4 entries", got)
	}
}

func TestLoaderRejectsUnknownFilterValue(t *testing.T) {
	t.Parallel()

	loader := newLoader(t, completeFS())

	_, err := loader.Load(context.Background(), "packages/demo/pips-bundle.json", bundle.Settings{
		Scope: bundle.ScopeUser,
		Filters: bundle.Filters{
			Extensions: bundle.ComponentFilter{Include: []string{"not-declared"}},
		},
	})
	if !errors.Is(err, bundle.ErrInvalid) {
		t.Fatalf("filter error = %v", err)
	}
}

func TestLoaderRequiresExplicitProjectTrust(t *testing.T) {
	t.Parallel()

	loader := newLoader(t, fstest.MapFS{})

	_, err := loader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{
		Scope: bundle.ScopeProject,
	})
	if !errors.Is(err, bundle.ErrUntrusted) {
		t.Fatalf("project trust error = %v", err)
	}

	_, err = loader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{
		Scope: bundle.ScopeUser,
		Trust: bundle.TrustDenied,
	})
	if !errors.Is(err, bundle.ErrUntrusted) {
		t.Fatalf("denied trust error = %v", err)
	}
}

func TestLoaderRejectsMalformedManifest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		manifest string
		want     error
	}{
		{
			name:     "unknown field",
			manifest: `{"schema":"pips.bundle/v1alpha1","id":"x","version":"1","command":"run"}`,
			want:     bundle.ErrInvalid,
		},
		{
			name:     "unsupported schema",
			manifest: `{"schema":"pips.bundle/v2","id":"x","version":"1"}`,
			want:     bundle.ErrUnsupportedSchema,
		},
		{
			name:     "traversal resource",
			manifest: `{"schema":"pips.bundle/v1alpha1","id":"x","version":"1","skills":["../outside"]}`,
			want:     bundle.ErrInvalid,
		},
		{
			name:     "duplicate identity",
			manifest: `{"schema":"pips.bundle/v1alpha1","id":"x","version":"1","extensions":["same","same"]}`,
			want:     bundle.ErrDuplicate,
		},
		{
			name:     "duplicate JSON key",
			manifest: `{"schema":"pips.bundle/v1alpha1","id":"x","id":"y","version":"1"}`,
			want:     bundle.ErrInvalid,
		},
		{
			name:     "non-canonical JSON key",
			manifest: `{"Schema":"pips.bundle/v1alpha1","id":"x","version":"1"}`,
			want:     bundle.ErrInvalid,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			loader := newLoader(t, fstest.MapFS{
				bundle.DefaultManifestPath: &fstest.MapFile{Data: []byte(test.manifest)},
			})

			_, err := loader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{Scope: bundle.ScopeUser})
			if !errors.Is(err, test.want) {
				t.Fatalf("manifest error = %v, want %v", err, test.want)
			}

			if test.name == "unknown field" && !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("manifest error = %v", err)
			}
		})
	}
}

func TestLoaderEnforcesManifestAndResourceLimits(t *testing.T) {
	t.Parallel()

	t.Run("manifest bytes", func(t *testing.T) {
		t.Parallel()

		limits := bundle.DefaultLimits()
		limits.MaxManifestBytes = 8

		loader, err := bundle.New(completeFS(), bundle.WithLimits(limits))
		if err != nil {
			t.Fatalf("new loader: %v", err)
		}

		_, err = loader.Load(context.Background(), "packages/demo/pips-bundle.json", bundle.Settings{Scope: bundle.ScopeUser})
		if !errors.Is(err, bundle.ErrLimitExceeded) {
			t.Fatalf("manifest limit error = %v", err)
		}
	})

	t.Run("single resource bytes", func(t *testing.T) {
		t.Parallel()

		limits := bundle.DefaultLimits()
		limits.MaxResourceBytes = 16

		loader, err := bundle.New(completeFS(), bundle.WithLimits(limits))
		if err != nil {
			t.Fatalf("new loader: %v", err)
		}

		_, err = loader.Load(context.Background(), "packages/demo/pips-bundle.json", bundle.Settings{Scope: bundle.ScopeUser})
		if !errors.Is(err, bundle.ErrLimitExceeded) {
			t.Fatalf("resource limit error = %v", err)
		}
	})

	t.Run("expanded resource count", func(t *testing.T) {
		t.Parallel()

		fsys := fstest.MapFS{
			bundle.DefaultManifestPath: &fstest.MapFile{Data: []byte(`{
				"schema":"pips.bundle/v1alpha1","id":"many","version":"1","skills":["skills"]
			}`)},
			"skills/one/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: one\ndescription: One.\n---\none")},
			"skills/two/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: two\ndescription: Two.\n---\ntwo")},
		}
		limits := bundle.DefaultLimits()
		limits.MaxResources = 1

		loader, err := bundle.New(fsys, bundle.WithLimits(limits))
		if err != nil {
			t.Fatalf("new loader: %v", err)
		}

		_, err = loader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{Scope: bundle.ScopeUser})
		if !errors.Is(err, bundle.ErrLimitExceeded) {
			t.Fatalf("resource count error = %v", err)
		}
	})

	t.Run("aggregate resource bytes", func(t *testing.T) {
		t.Parallel()

		fsys := fstest.MapFS{
			bundle.DefaultManifestPath: &fstest.MapFile{Data: []byte(`{
				"schema":"pips.bundle/v1alpha1","id":"aggregate","version":"1",
				"assets":[
					{"kind":"data","name":"one","path":"one.bin"},
					{"kind":"data","name":"two","path":"two.bin"}
				]
			}`)},
			"one.bin": &fstest.MapFile{Data: []byte("12345678")},
			"two.bin": &fstest.MapFile{Data: []byte("12345678")},
		}
		limits := bundle.DefaultLimits()
		limits.MaxResourceBytes = 8
		limits.MaxTotalBytes = 12

		loader, err := bundle.New(fsys, bundle.WithLimits(limits))
		if err != nil {
			t.Fatalf("new loader: %v", err)
		}

		_, err = loader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{Scope: bundle.ScopeUser})
		if !errors.Is(err, bundle.ErrLimitExceeded) {
			t.Fatalf("aggregate limit error = %v", err)
		}
	})
}

func TestLoaderRejectsFilesystemEscapes(t *testing.T) {
	t.Parallel()

	loader := newLoader(t, completeFS())

	_, err := loader.Load(context.Background(), "../pips-bundle.json", bundle.Settings{Scope: bundle.ScopeUser})
	if !errors.Is(err, bundle.ErrInvalid) {
		t.Fatalf("manifest traversal error = %v", err)
	}

	root := t.TempDir()

	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside asset: %v", err)
	}

	if err := os.Symlink(outside, filepath.Join(root, "escape.json")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	manifest := `{
		"schema":"pips.bundle/v1alpha1","id":"escape","version":"1",
		"assets":[{"kind":"data","name":"escape","path":"escape.json"}]
	}`
	if err := os.WriteFile(filepath.Join(root, bundle.DefaultManifestPath), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	diskLoader, err := bundle.Open(root)
	if err != nil {
		t.Fatalf("open loader: %v", err)
	}

	t.Cleanup(func() { _ = diskLoader.Close() })

	_, err = diskLoader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{Scope: bundle.ScopeUser})
	if err == nil {
		t.Fatal("external symlink asset was loaded")
	}
}

func TestLoaderCloseRejectsFurtherLoads(t *testing.T) {
	t.Parallel()

	loader, err := bundle.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open loader: %v", err)
	}

	if err := loader.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := loader.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	_, err = loader.Load(context.Background(), bundle.DefaultManifestPath, bundle.Settings{Scope: bundle.ScopeUser})
	if !errors.Is(err, bundle.ErrClosed) {
		t.Fatalf("load after close error = %v", err)
	}
}

func newLoader(t *testing.T, fsys fstest.MapFS) *bundle.Loader {
	t.Helper()

	loader, err := bundle.New(fsys)
	if err != nil {
		t.Fatalf("new loader: %v", err)
	}

	return loader
}

func completeFS() fstest.MapFS {
	return fstest.MapFS{
		"packages/demo/pips-bundle.json": &fstest.MapFile{Data: []byte(`{
			"schema":"pips.bundle/v1alpha1",
			"id":"demo",
			"version":"1.2.3",
			"requires":["harness.skills"],
			"optional":["application.optional"],
			"extensions":["compiled"],
			"skills":["skills/review/SKILL.md"],
			"prompts":["prompts"],
			"assets":[{"kind":"theme","name":"default","path":"assets/theme.json","media_type":"application/json"}]
		}`)},
		"packages/demo/skills/review/SKILL.md": &fstest.MapFile{Data: []byte(`---
name: review
description: Review a code change.
metadata:
  owner: core
---
Review carefully.`)},
		"packages/demo/prompts/summarize.md": &fstest.MapFile{Data: []byte(`---
name: summarize
description: Summarize a value.
---
Summarize $1.`)},
		"packages/demo/assets/theme.json": &fstest.MapFile{Data: []byte(`{"color":"blue"}`)},
	}
}
