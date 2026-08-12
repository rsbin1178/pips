package workflow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin1178/pips/workflow"
)

type fakeAction struct {
	spec workflow.ActionSpec
	run  func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error)
}

func (a *fakeAction) Spec() workflow.ActionSpec {
	return a.spec
}

func (a *fakeAction) Run(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
	if a.run != nil {
		return a.run(ctx, input)
	}

	return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
}

func TestNewRegistry(t *testing.T) {
	t.Parallel()

	stringSchema, err := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	if err != nil {
		t.Fatalf("ParsePortSchema() error = %v", err)
	}

	action := &fakeAction{spec: workflow.ActionSpec{
		Key:     "save",
		Version: "v1",
		Inputs:  map[string]workflow.PortSchema{"value": stringSchema},
		Outputs: map[string]workflow.PortSchema{},
	}}

	registry, err := workflow.NewRegistry(nil, []workflow.Action{action})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	if registry.Fingerprint() == "" {
		t.Fatal("Registry.Fingerprint() is empty")
	}

	if resolved, ok := registry.Action("save", "v1"); !ok || resolved.Spec().Key != "save" {
		t.Fatalf("Registry.Action() = %T, %v, want save action, true", resolved, ok)
	}
}

func TestNewRegistryRejectsDuplicateAndTypedNilAction(t *testing.T) {
	t.Parallel()

	emptySchema := map[string]workflow.PortSchema{}
	action := &fakeAction{spec: workflow.ActionSpec{
		Key: "save", Version: "v1", Inputs: emptySchema, Outputs: emptySchema,
	}}

	tests := []struct {
		name    string
		actions []workflow.Action
	}{
		{name: "duplicate", actions: []workflow.Action{action, action}},
		{name: "typed nil", actions: []workflow.Action{(*fakeAction)(nil)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := workflow.NewRegistry(nil, test.actions)
			if !errors.Is(err, workflow.ErrInvalidRegistry) {
				t.Fatalf("NewRegistry() error = %v, want ErrInvalidRegistry", err)
			}
		})
	}
}

func TestNewRegistryRejectsDuplicateNodeType(t *testing.T) {
	t.Parallel()

	_, err := workflow.NewRegistry(
		[]workflow.NodeType{workflow.StartNode{}, workflow.StartNode{}},
		nil,
	)
	if !errors.Is(err, workflow.ErrInvalidRegistry) {
		t.Fatalf("NewRegistry() error = %v, want ErrInvalidRegistry", err)
	}
}

func TestRegistrySnapshotsActionSpec(t *testing.T) {
	t.Parallel()

	stringSchema, err := workflow.ParsePortSchema([]byte(`{"type":"string"}`))
	if err != nil {
		t.Fatalf("ParsePortSchema() error = %v", err)
	}

	inputs := map[string]workflow.PortSchema{"value": stringSchema}
	outputs := map[string]workflow.PortSchema{"result": stringSchema}
	action := &fakeAction{spec: workflow.ActionSpec{
		Key: "snapshot", Version: "v1", Inputs: inputs, Outputs: outputs,
	}}

	registry, err := workflow.NewRegistry(nil, []workflow.Action{action})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	fingerprint := registry.Fingerprint()

	delete(inputs, "value")
	delete(outputs, "result")

	action.spec.Key = "mutated"

	resolved, ok := registry.Action("snapshot", "v1")
	if !ok {
		t.Fatal("Registry.Action() did not resolve snapshotted action")
	}

	spec := resolved.Spec()
	delete(spec.Inputs, "value")

	second := resolved.Spec()
	if second.Key != "snapshot" || len(second.Inputs) != 1 || len(second.Outputs) != 1 {
		t.Fatalf("snapshotted spec changed: %#v", second)
	}

	if registry.Fingerprint() != fingerprint {
		t.Fatal("Registry fingerprint changed after source mutation")
	}
}

func TestDefaultRegistryAddsMergeV2WithoutChangingV1PlanContract(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition, actions := directMergeDefinition(t, workflow.BuiltinNodeVersion, stringSchema)
	builtins := workflow.BuiltinNodeTypes()
	v1Only := make([]workflow.NodeType, 0, len(builtins)-1)
	families := make(map[workflow.NodeTypeKey]struct{})

	for _, nodeType := range builtins {
		spec := nodeType.Spec()

		families[spec.Key] = struct{}{}
		if spec.Key != workflow.NodeTypeMerge || spec.Version != workflow.MergeNodeVersionV2 {
			v1Only = append(v1Only, nodeType)
		}
	}

	if len(builtins) != 13 || len(families) != 12 {
		t.Fatalf("built-in registrations/families = %d/%d, want 13/12", len(builtins), len(families))
	}

	fullRegistry, err := workflow.NewRegistry(builtins, actions)
	if err != nil {
		t.Fatalf("NewRegistry(full) error = %v", err)
	}

	oldRegistry, err := workflow.NewRegistry(v1Only, actions)
	if err != nil {
		t.Fatalf("NewRegistry(v1 only) error = %v", err)
	}

	for _, version := range []string{workflow.BuiltinNodeVersion, workflow.MergeNodeVersionV2} {
		if _, ok := fullRegistry.NodeType(workflow.NodeTypeMerge, version); !ok {
			t.Fatalf("full Registry does not resolve merge@%s", version)
		}
	}

	if fullRegistry.Fingerprint() == oldRegistry.Fingerprint() {
		t.Fatal("Registry fingerprint did not change after merge@v2 registration")
	}

	fullPlan, err := workflow.Compile(t.Context(), definition, fullRegistry)
	if err != nil {
		t.Fatalf("Compile(full) error = %v", err)
	}

	oldPlan, err := workflow.Compile(t.Context(), definition, oldRegistry)
	if err != nil {
		t.Fatalf("Compile(v1 only) error = %v", err)
	}

	if fullPlan.Fingerprint() != oldPlan.Fingerprint() {
		t.Fatalf("unused merge@v2 changed v1 plan contract: %s != %s", fullPlan.Fingerprint(), oldPlan.Fingerprint())
	}
}
