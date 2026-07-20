package bundle_test

import (
	"context"
	"fmt"
	"testing/fstest"

	"github.com/rsbin/pips/agent/bundle"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/rsbin/pips/agent/extension"
)

func ExampleBundle_Activate() {
	ctx := context.Background()
	loader, _ := bundle.New(fstest.MapFS{
		"pips-bundle.json": &fstest.MapFile{Data: []byte(`{
			"schema":"pips.bundle/v1alpha1",
			"id":"project-review",
			"version":"1.0.0",
			"extensions":["audit"],
			"skills":["skills/review/SKILL.md"]
		}`)},
		"skills/review/SKILL.md": &fstest.MapFile{Data: []byte(`---
name: review
description: Review a code change.
---
Review carefully.`)},
		// Bundled scripts remain ordinary files and are never registered as Tools.
		"skills/review/scripts/check.sh": &fstest.MapFile{Data: []byte("exit 0")},
	})

	bundle, _ := loader.Load(ctx, bundle.DefaultManifestPath, bundle.Settings{
		Scope: bundle.ScopeProject,
		Trust: bundle.TrustApproved,
	})
	audit, _ := extension.NewDefinition(extension.Descriptor{
		ID: "audit", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{}, nil
	})
	runtime, _ := extension.New(extension.WithExtensions(audit))
	activation, _ := bundle.Activate(ctx, runtime)
	snapshot := activation.Snapshot()
	tools, _ := snapshot.Catalog().Snapshot(ctx, catalog.AllowAll("example", catalog.RiskPrivileged))

	fmt.Println(snapshot.Generation())
	fmt.Println(snapshot.Descriptors()[0].ID, snapshot.Descriptors()[1].ID)
	fmt.Println(snapshot.Skills()[0].Name, len(tools))

	_ = activation.Release(ctx)
	_ = runtime.Shutdown(ctx)

	// Output:
	// 1
	// audit bundle:project-review
	// review 0
}
