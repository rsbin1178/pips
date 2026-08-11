package workflow_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/workflow"
)

const (
	legacyGoldenRegistryFingerprint = "69c83a2b0a7949458e64d0b1c27c1493625a5e91e25af5909be59f3a2eb700a5"
	legacyGoldenRootPlanFingerprint = "4c0fe32098024a1ebeac3d9eddc7aaeaf9dd58f1e96bdfde56773e333d516c0c"
	legacyGoldenNestedFingerprint   = "31af655c22ffdd17e6542d4aa460ccea129b4473902d4468e3476d2fe7344b8e"
)

func TestCheckpointV3UsesReferencedContractIdentity(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name           string
		writeExpanded  bool
		resumeExpanded bool
	}{
		{name: "add unused contracts", resumeExpanded: true},
		{name: "remove unused contracts", writeExpanded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32

			stringSchema := mustSchema(t, `{"type":"string"}`)
			usedAction := resultAction(
				"checkpoint_used",
				stringSchema,
				func(context.Context) (workflow.Value, error) {
					calls.Add(1)

					return workflow.MustValueOf("used"), nil
				},
			)
			definition := singleActionDefinition(
				t,
				"checkpoint_used",
				stringSchema,
				workflow.NodePolicy{},
			)

			baseRegistry, err := workflow.NewDefaultRegistry(usedAction)
			if err != nil {
				t.Fatalf("NewDefaultRegistry() error = %v", err)
			}

			expandedRegistry := newCheckpointExpandedRegistry(t, usedAction)
			basePlan := compileCheckpointIdentityPlan(t, definition, baseRegistry)
			expandedPlan := compileCheckpointIdentityPlan(t, definition, expandedRegistry)

			if basePlan.Fingerprint() != expandedPlan.Fingerprint() {
				t.Fatal("unused Registry contracts changed the current Plan fingerprint")
			}

			if basePlan.RegistryFingerprint() == expandedPlan.RegistryFingerprint() {
				t.Fatal("unused Registry contracts did not change the full Registry fingerprint")
			}

			writePlan := basePlan
			resumePlan := basePlan

			if test.writeExpanded {
				writePlan = expandedPlan
			}

			if test.resumeExpanded {
				resumePlan = expandedPlan
			}

			store := &memoryCheckpointStore{}
			runner := checkpointIdentityRunner(t, store, "checkpoint-v3-unused")
			result, err := runner.Run(t.Context(), writePlan, map[string]workflow.Value{})
			assertInterrupted(t, result, err, "checkpoint-v3-unused")

			assertCurrentCheckpointHeader(t, store.value("checkpoint-v3-unused"), writePlan)

			resumed, err := runner.Resume(
				t.Context(),
				resumePlan,
				"checkpoint-v3-unused",
				nil,
			)
			if err != nil {
				t.Fatalf("Runner.Resume() error = %v", err)
			}

			if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 {
				t.Fatalf("resumed status = %q, calls = %d", resumed.Status, calls.Load())
			}
		})
	}
}

func TestCheckpointV3RejectsUsedContractChangeBeforeInvocation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	stringSchema := mustSchema(t, `{"type":"string"}`)
	definition := singleActionDefinition(
		t,
		"checkpoint_changed",
		stringSchema,
		workflow.NodePolicy{},
	)
	baseAction := resultAction(
		"checkpoint_changed",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			calls.Add(1)

			return workflow.MustValueOf("base"), nil
		},
	)
	changedAction := &fakeAction{
		spec: actionSpec(
			"checkpoint_changed",
			map[string]workflow.PortSchema{},
			map[string]workflow.PortSchema{
				"result": stringSchema,
				"extra":  stringSchema,
			},
		),
		run: func(context.Context, workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": workflow.MustValueOf("changed"),
				"extra":  workflow.MustValueOf("changed"),
			}}, nil
		},
	}

	baseRegistry, err := workflow.NewDefaultRegistry(baseAction)
	if err != nil {
		t.Fatalf("NewDefaultRegistry(base) error = %v", err)
	}

	changedRegistry, err := workflow.NewDefaultRegistry(changedAction)
	if err != nil {
		t.Fatalf("NewDefaultRegistry(changed) error = %v", err)
	}

	basePlan := compileCheckpointIdentityPlan(t, definition, baseRegistry)

	changedPlan := compileCheckpointIdentityPlan(t, definition, changedRegistry)
	if basePlan.Fingerprint() == changedPlan.Fingerprint() {
		t.Fatal("used Action contract change did not change the Plan fingerprint")
	}

	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v3-changed")
	result, err := runner.Run(t.Context(), basePlan, map[string]workflow.Value{})
	assertInterrupted(t, result, err, "checkpoint-v3-changed")

	_, err = runner.Resume(t.Context(), changedPlan, "checkpoint-v3-changed", nil)
	if !errors.Is(err, workflow.ErrRun) || errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("Runner.Resume() error = %v, want ErrRun only", err)
	}

	if calls.Load() != 0 {
		t.Fatalf("Action calls after identity rejection = %d, want 0", calls.Load())
	}
}

func TestCheckpointV3RejectsNodeAndOptionalLookupChangesBeforeInvocation(t *testing.T) {
	t.Parallel()

	t.Run("compiled NodeSpec", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32

		baseNodeType := fingerprintNodeType{
			key:    "checkpoint_node_contract",
			invoke: func() { calls.Add(1) },
		}
		stringSchema := mustSchema(t, `{"type":"string"}`)
		changedNodeType := fingerprintNodeType{
			key:    "checkpoint_node_contract",
			invoke: func() { calls.Add(1) },
			compileNode: func(workflow.CompileContext) workflow.NodeSpec {
				return workflow.NodeSpec{
					Inputs:  map[string]workflow.PortSchema{},
					Outputs: map[string]workflow.PortSchema{"result": stringSchema},
					Routes:  []string{workflow.RouteSuccess},
				}
			},
		}
		definition := fingerprintNodeDefinition("checkpoint_node_contract")
		basePlan := compileCustomCheckpointPlan(
			t,
			definition,
			newFingerprintRegistry(t, baseNodeType, nil),
		)
		changedPlan := compileCustomCheckpointPlan(
			t,
			definition,
			newFingerprintRegistry(t, changedNodeType, nil),
		)

		assertCheckpointIdentityRejected(t, basePlan, changedPlan, &calls, "checkpoint-node-contract")
	})

	t.Run("optional Action absence becomes presence", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32

		nodeType := fingerprintNodeType{
			key:    "checkpoint_optional_lookup",
			invoke: func() { calls.Add(1) },
			compileNode: func(compileContext workflow.CompileContext) workflow.NodeSpec {
				compileContext.Action("checkpoint_optional", "v1")

				return emptyFingerprintNodeSpec()
			},
		}
		definition := fingerprintNodeDefinition("checkpoint_optional_lookup")
		basePlan := compileCustomCheckpointPlan(
			t,
			definition,
			newFingerprintRegistry(t, nodeType, nil),
		)
		presentPlan := compileCustomCheckpointPlan(
			t,
			definition,
			newFingerprintRegistry(t, nodeType, []workflow.Action{
				fingerprintAction("checkpoint_optional", mustSchema(t, `{"type":"string"}`)),
			}),
		)

		assertCheckpointIdentityRejected(
			t,
			basePlan,
			presentPlan,
			&calls,
			"checkpoint-optional-lookup",
		)
	})
}

func TestCheckpointV2ReadsExactLegacyIdentity(t *testing.T) {
	t.Parallel()

	t.Run("same full Registry resumes", func(t *testing.T) {
		t.Parallel()

		plan, action, calls := legacyGoldenCheckpointPlan(t)
		store := &memoryCheckpointStore{}
		runner := checkpointIdentityRunner(t, store, "checkpoint-v2-same")
		result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
		assertInterrupted(t, result, err, "checkpoint-v2-same")
		store.replace(
			"checkpoint-v2-same",
			asLegacyGoldenCheckpoint(t, store.value("checkpoint-v2-same")),
		)

		resumed, err := runner.Resume(t.Context(), plan, "checkpoint-v2-same", nil)
		if err != nil {
			t.Fatalf("Runner.Resume() error = %v", err)
		}

		if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 || action == nil {
			t.Fatalf("resumed status = %q, calls = %d", resumed.Status, calls.Load())
		}
	})

	t.Run("changed full Registry is rejected", func(t *testing.T) {
		t.Parallel()

		plan, action, calls := legacyGoldenCheckpointPlan(t)
		store := &memoryCheckpointStore{}
		runner := checkpointIdentityRunner(t, store, "checkpoint-v2-changed")
		result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
		assertInterrupted(t, result, err, "checkpoint-v2-changed")
		store.replace(
			"checkpoint-v2-changed",
			asLegacyGoldenCheckpoint(t, store.value("checkpoint-v2-changed")),
		)

		expandedRegistry := newCheckpointExpandedRegistry(t, action)
		stringSchema := mustSchema(t, `{"type":"string"}`)
		definition := singleActionDefinition(
			t,
			"legacy_golden_action",
			stringSchema,
			workflow.NodePolicy{},
		)

		expandedPlan := compileCheckpointIdentityPlan(
			t,
			definition,
			expandedRegistry,
		)
		if plan.Fingerprint() != expandedPlan.Fingerprint() {
			t.Fatal("unused Registry contract changed current Plan fingerprint")
		}

		_, err = runner.Resume(t.Context(), expandedPlan, "checkpoint-v2-changed", nil)
		if !errors.Is(err, workflow.ErrRun) || errors.Is(err, workflow.ErrInterrupted) {
			t.Fatalf("Runner.Resume() error = %v, want ErrRun only", err)
		}

		if calls.Load() != 0 {
			t.Fatalf("Action calls after v2 Registry rejection = %d, want 0", calls.Load())
		}
	})
}

func TestCheckpointRejectsCrossVersionIdentityFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "v3 contains Registry fingerprint",
			mutate: func(checkpoint map[string]any) {
				checkpoint["registry_fingerprint"] = legacyGoldenRegistryFingerprint
			},
		},
		{
			name: "v3 contains empty Registry fingerprint",
			mutate: func(checkpoint map[string]any) {
				checkpoint["registry_fingerprint"] = ""
			},
		},
		{
			name: "v3 misses contract fingerprint",
			mutate: func(checkpoint map[string]any) {
				delete(checkpoint, "contract_fingerprint")
			},
		},
		{
			name: "v2 contains contract fingerprint",
			mutate: func(checkpoint map[string]any) {
				checkpoint["version"] = float64(2)
				checkpoint["registry_fingerprint"] = legacyGoldenRegistryFingerprint
				checkpoint["plan_fingerprint"] = legacyGoldenRootPlanFingerprint
				execution := checkpointExecution(t, checkpoint)
				execution["plan_fingerprint"] = legacyGoldenRootPlanFingerprint
			},
		},
		{
			name: "v2 contains empty contract fingerprint",
			mutate: func(checkpoint map[string]any) {
				checkpoint["version"] = float64(2)
				checkpoint["contract_fingerprint"] = ""
				checkpoint["registry_fingerprint"] = legacyGoldenRegistryFingerprint
				checkpoint["plan_fingerprint"] = legacyGoldenRootPlanFingerprint
				execution := checkpointExecution(t, checkpoint)
				execution["plan_fingerprint"] = legacyGoldenRootPlanFingerprint
			},
		},
		{
			name: "v2 misses Registry fingerprint",
			mutate: func(checkpoint map[string]any) {
				checkpoint["version"] = float64(2)
				delete(checkpoint, "contract_fingerprint")
				delete(checkpoint, "registry_fingerprint")
				checkpoint["plan_fingerprint"] = legacyGoldenRootPlanFingerprint
				execution := checkpointExecution(t, checkpoint)
				execution["plan_fingerprint"] = legacyGoldenRootPlanFingerprint
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan, _, calls := legacyGoldenCheckpointPlan(t)
			store := &memoryCheckpointStore{}
			runner := checkpointIdentityRunner(t, store, "checkpoint-cross-mode")
			result, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
			assertInterrupted(t, result, err, "checkpoint-cross-mode")
			store.replace(
				"checkpoint-cross-mode",
				mutateCheckpointJSON(t, store.value("checkpoint-cross-mode"), test.mutate),
			)

			_, err = runner.Resume(t.Context(), plan, "checkpoint-cross-mode", nil)
			if !errors.Is(err, workflow.ErrRun) || errors.Is(err, workflow.ErrInterrupted) {
				t.Fatalf("Runner.Resume() error = %v, want ErrRun only", err)
			}

			if calls.Load() != 0 {
				t.Fatalf("Action calls after invalid identity fields = %d, want 0", calls.Load())
			}
		})
	}
}

func TestCheckpointV2ReadsLegacyNodeDebugIdentity(t *testing.T) {
	t.Parallel()

	plan, _, calls := legacyGoldenCheckpointPlan(t)

	debugPlan, err := workflow.PrepareNodeDebug(plan, workflow.NewNodePath("action"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug() error = %v", err)
	}

	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v2-debug")

	interrupted, err := runner.DebugNode(t.Context(), debugPlan, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrInterrupted) || interrupted.Status != workflow.RunStatusInterrupted {
		t.Fatalf("DebugNode() result = %#v, error = %v", interrupted, err)
	}

	legacyDebugFingerprint := legacyGoldenNodeDebugFingerprint(
		t,
		plan,
		debugPlan,
		legacyGoldenDefinition(t).Nodes[1],
	)
	store.replace(
		"checkpoint-v2-debug",
		mutateCheckpointJSON(t, store.value("checkpoint-v2-debug"), func(checkpoint map[string]any) {
			checkpoint["version"] = float64(2)
			delete(checkpoint, "contract_fingerprint")
			checkpoint["registry_fingerprint"] = legacyGoldenRegistryFingerprint
			checkpoint["plan_fingerprint"] = legacyDebugFingerprint
			checkpointExecution(t, checkpoint)["plan_fingerprint"] = legacyDebugFingerprint

			nodeDebug, ok := checkpoint["node_debug"].(map[string]any)
			if !ok {
				t.Fatal("checkpoint node_debug is not an object")
			}

			nodeDebug["fingerprint"] = legacyDebugFingerprint
		}),
	)

	resumed, err := runner.ResumeNodeDebug(
		t.Context(),
		debugPlan,
		"checkpoint-v2-debug",
		nil,
	)
	if err != nil {
		t.Fatalf("ResumeNodeDebug() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("resumed status = %q, calls = %d", resumed.Status, calls.Load())
	}
}

func TestCheckpointV2ReadsLegacyPartialRunIdentity(t *testing.T) {
	t.Parallel()

	plan, _, calls := legacyGoldenCheckpointPlan(t)
	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v2-partial")
	interrupted, err := runner.RunPartial(
		t.Context(),
		plan,
		"action",
		workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
	)
	assertPartialInterrupted(t, interrupted, err, "checkpoint-v2-partial")

	legacyPartialFingerprint := legacyFingerprintValue(t, struct {
		Strategy    string          `json:"strategy"`
		Source      string          `json:"source"`
		Destination workflow.NodeID `json:"destination"`
	}{
		Strategy:    "pips.workflow/partial-run-plan/v1",
		Source:      legacyGoldenRootPlanFingerprint,
		Destination: "action",
	})
	store.replace(
		"checkpoint-v2-partial",
		mutateCheckpointJSON(t, store.value("checkpoint-v2-partial"), func(checkpoint map[string]any) {
			checkpoint["version"] = float64(2)
			delete(checkpoint, "contract_fingerprint")
			checkpoint["registry_fingerprint"] = legacyGoldenRegistryFingerprint
			checkpoint["plan_fingerprint"] = legacyPartialFingerprint
			checkpointExecution(t, checkpoint)["plan_fingerprint"] = legacyPartialFingerprint

			partialRun, ok := checkpoint["partial_run"].(map[string]any)
			if !ok {
				t.Fatal("checkpoint partial_run is not an object")
			}

			partialRun["source_plan_fingerprint"] = legacyGoldenRootPlanFingerprint
		}),
	)

	resumed, err := runner.ResumePartial(
		t.Context(),
		plan,
		"action",
		"checkpoint-v2-partial",
		nil,
	)
	if err != nil {
		t.Fatalf("ResumePartial() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("resumed status = %q, calls = %d", resumed.Status, calls.Load())
	}
}

func TestCheckpointV2ReadsRecursiveLegacyIdentity(t *testing.T) {
	t.Parallel()

	plan, _, calls := nestedCheckpointIdentityPlan(t, false)
	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v2-nested")
	interrupted, err := runner.Run(t.Context(), plan, map[string]workflow.Value{
		"value": workflow.MustValueOf("input"),
	})
	assertInterrupted(t, interrupted, err, "checkpoint-v2-nested")

	if calls.Load() != 1 || len(interrupted.Interruption.Contexts) != 1 {
		t.Fatalf("nested interruption = %#v, calls = %d", interrupted.Interruption, calls.Load())
	}

	childDefinition := subWorkflowChildDefinition(
		mustSchema(t, `{"type":"string"}`),
		"legacy_child_action",
	)

	childDefinitionFingerprint, err := childDefinition.Fingerprint()
	if err != nil {
		t.Fatalf("child Definition.Fingerprint() error = %v", err)
	}

	legacyChildFingerprint := legacyFingerprintValue(t, struct {
		Definition string `json:"definition"`
		Registry   string `json:"registry"`
	}{
		Definition: childDefinitionFingerprint,
		Registry:   plan.RegistryFingerprint(),
	})
	store.replace(
		"checkpoint-v2-nested",
		mutateCheckpointJSON(t, store.value("checkpoint-v2-nested"), func(checkpoint map[string]any) {
			checkpoint["version"] = float64(2)
			delete(checkpoint, "contract_fingerprint")
			checkpoint["registry_fingerprint"] = plan.RegistryFingerprint()
			checkpoint["plan_fingerprint"] = legacyGoldenNestedFingerprint

			execution := checkpointExecution(t, checkpoint)
			execution["plan_fingerprint"] = legacyGoldenNestedFingerprint
			childExecution := checkpointPausedChild(t, execution)
			childExecution["plan_fingerprint"] = legacyChildFingerprint
		}),
	)

	context := interrupted.Interruption.Contexts[0]

	resumed, err := runner.Resume(
		t.Context(),
		plan,
		"checkpoint-v2-nested",
		[]workflow.ResumeTarget{{
			InterruptID: context.ID,
			Data:        workflow.MustValueOf("resumed"),
		}},
	)
	if err != nil {
		t.Fatalf("Runner.Resume() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded ||
		resumed.Outputs["result"].String() != `"resumed"` || calls.Load() != 2 {
		t.Fatalf("resumed result = %#v, calls = %d", resumed, calls.Load())
	}
}

func TestCheckpointV2ResumeWritesNextInterruptionAsV3(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	action := resultAction(
		"checkpoint_rewrite",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			calls.Add(1)

			return workflow.MustValueOf("rewritten"), nil
		},
	)

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	definition := singleActionDefinition(
		t,
		"checkpoint_rewrite",
		stringSchema,
		workflow.NodePolicy{},
	)

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("action")),
		workflow.WithInterruptAfterNodes(workflow.NewNodePath("action")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	legacyPlanFingerprint := legacyFingerprintValue(t, struct {
		Definition      string            `json:"definition"`
		Registry        string            `json:"registry"`
		InterruptBefore []workflow.NodeID `json:"interrupt_before,omitempty"`
		InterruptAfter  []workflow.NodeID `json:"interrupt_after,omitempty"`
	}{
		Definition:      plan.DefinitionFingerprint(),
		Registry:        plan.RegistryFingerprint(),
		InterruptBefore: []workflow.NodeID{"action"},
		InterruptAfter:  []workflow.NodeID{"action"},
	})

	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v2-rewrite")
	interrupted, err := runner.Run(t.Context(), plan, map[string]workflow.Value{})
	assertInterrupted(t, interrupted, err, "checkpoint-v2-rewrite")
	store.replace(
		"checkpoint-v2-rewrite",
		mutateCheckpointJSON(t, store.value("checkpoint-v2-rewrite"), func(checkpoint map[string]any) {
			checkpoint["version"] = float64(2)
			delete(checkpoint, "contract_fingerprint")
			checkpoint["registry_fingerprint"] = plan.RegistryFingerprint()
			checkpoint["plan_fingerprint"] = legacyPlanFingerprint
			checkpointExecution(t, checkpoint)["plan_fingerprint"] = legacyPlanFingerprint
		}),
	)

	interrupted, err = runner.Resume(
		t.Context(),
		plan,
		"checkpoint-v2-rewrite",
		nil,
	)
	assertInterrupted(t, interrupted, err, "checkpoint-v2-rewrite")

	if calls.Load() != 1 {
		t.Fatalf("Action calls after v2 resume = %d, want 1", calls.Load())
	}

	assertCurrentCheckpointHeader(t, store.value("checkpoint-v2-rewrite"), plan)
}

func TestCheckpointV2PartialResumeNormalizesNextV3Identity(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)

	var calls atomic.Int32

	action := resultAction(
		"partial_rewrite",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			calls.Add(1)

			return workflow.MustValueOf("rewritten"), nil
		},
	)

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	definition := singleActionDefinition(
		t,
		"partial_rewrite",
		stringSchema,
		workflow.NodePolicy{},
	)

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("action")),
		workflow.WithInterruptAfterNodes(workflow.NewNodePath("action")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	legacySourceFingerprint := legacyFingerprintValue(t, struct {
		Definition      string            `json:"definition"`
		Registry        string            `json:"registry"`
		InterruptBefore []workflow.NodeID `json:"interrupt_before,omitempty"`
		InterruptAfter  []workflow.NodeID `json:"interrupt_after,omitempty"`
	}{
		Definition:      plan.DefinitionFingerprint(),
		Registry:        plan.RegistryFingerprint(),
		InterruptBefore: []workflow.NodeID{"action"},
		InterruptAfter:  []workflow.NodeID{"action"},
	})
	legacyPartialFingerprint := legacyFingerprintValue(t, struct {
		Strategy    string          `json:"strategy"`
		Source      string          `json:"source"`
		Destination workflow.NodeID `json:"destination"`
	}{
		Strategy:    "pips.workflow/partial-run-plan/v1",
		Source:      legacySourceFingerprint,
		Destination: "action",
	})

	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v2-partial-rewrite")
	interrupted, err := runner.RunPartial(
		t.Context(),
		plan,
		"action",
		workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
	)
	assertPartialInterrupted(t, interrupted, err, "checkpoint-v2-partial-rewrite")
	store.replace(
		"checkpoint-v2-partial-rewrite",
		mutateCheckpointJSON(
			t,
			store.value("checkpoint-v2-partial-rewrite"),
			func(checkpoint map[string]any) {
				checkpoint["version"] = float64(2)
				delete(checkpoint, "contract_fingerprint")
				checkpoint["registry_fingerprint"] = plan.RegistryFingerprint()
				checkpoint["plan_fingerprint"] = legacyPartialFingerprint
				checkpointExecution(t, checkpoint)["plan_fingerprint"] = legacyPartialFingerprint

				partialRun, ok := checkpoint["partial_run"].(map[string]any)
				if !ok {
					t.Fatal("checkpoint partial_run is not an object")
				}

				partialRun["source_plan_fingerprint"] = legacySourceFingerprint
			},
		),
	)

	interrupted, err = runner.ResumePartial(
		t.Context(),
		plan,
		"action",
		"checkpoint-v2-partial-rewrite",
		nil,
	)
	assertPartialInterrupted(t, interrupted, err, "checkpoint-v2-partial-rewrite")

	if calls.Load() != 1 {
		t.Fatalf("Action calls after v2 Partial resume = %d, want 1", calls.Load())
	}

	var checkpoint map[string]any
	if err := json.Unmarshal(store.value("checkpoint-v2-partial-rewrite"), &checkpoint); err != nil {
		t.Fatalf("json.Unmarshal(checkpoint) error = %v", err)
	}

	if checkpoint["version"] != float64(3) ||
		checkpoint["plan_fingerprint"] != interrupted.PlanFingerprint {
		t.Fatalf("rewritten Partial checkpoint identity = %#v", checkpoint)
	}

	if _, exists := checkpoint["registry_fingerprint"]; exists {
		t.Fatal("rewritten Partial checkpoint contains registry_fingerprint")
	}

	partialRun, ok := checkpoint["partial_run"].(map[string]any)
	if !ok || partialRun["source_plan_fingerprint"] != plan.Fingerprint() {
		t.Fatalf("rewritten Partial source identity = %#v", partialRun)
	}
}

func TestCheckpointV3ResumesRecursiveExecutionAfterUnusedContracts(t *testing.T) {
	t.Parallel()

	basePlan, action, calls := nestedCheckpointIdentityPlan(t, false)

	expandedPlan, _, _ := nestedCheckpointIdentityPlanWithAction(t, action, true)
	if basePlan.Fingerprint() != expandedPlan.Fingerprint() ||
		basePlan.RegistryFingerprint() == expandedPlan.RegistryFingerprint() {
		t.Fatalf(
			"nested identities current=%q/%q Registry=%q/%q",
			basePlan.Fingerprint(),
			expandedPlan.Fingerprint(),
			basePlan.RegistryFingerprint(),
			expandedPlan.RegistryFingerprint(),
		)
	}

	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v3-nested")
	interrupted, err := runner.Run(t.Context(), basePlan, map[string]workflow.Value{
		"value": workflow.MustValueOf("input"),
	})
	assertInterrupted(t, interrupted, err, "checkpoint-v3-nested")
	context := interrupted.Interruption.Contexts[0]

	resumed, err := runner.Resume(
		t.Context(),
		expandedPlan,
		"checkpoint-v3-nested",
		[]workflow.ResumeTarget{{
			InterruptID: context.ID,
			Data:        workflow.MustValueOf("resumed"),
		}},
	)
	if err != nil {
		t.Fatalf("Runner.Resume() error = %v", err)
	}

	if resumed.Outputs["result"].String() != `"resumed"` || calls.Load() != 2 {
		t.Fatalf("resumed result = %#v, calls = %d", resumed, calls.Load())
	}
}

func TestCheckpointV3ResumesBatchAfterUnusedContracts(t *testing.T) {
	t.Parallel()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	integerSchema := mustSchema(t, `{"type":"integer"}`)
	itemsSchema := mustSchema(t, `{"type":"array","items":{"type":"string"}}`)

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"checkpoint_batch_scoped",
			map[string]workflow.PortSchema{
				"item":  stringSchema,
				"index": integerSchema,
			},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			isTarget, _, _ := workflow.GetResumeContext(ctx)
			if !isTarget {
				return workflow.ActionOutput{}, workflow.Interrupt(ctx, input.Values["index"])
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": input.Values["item"],
			}}, nil
		},
	}
	body := batchBodyDefinition(t, stringSchema, stringSchema, "checkpoint_batch_scoped", false)
	definition := batchParentDefinition(
		t,
		itemsSchema,
		itemsSchema,
		workflow.BatchConfig{
			Body: body, ResultOutput: "result", Mode: workflow.BatchSequential,
			ErrorMode: workflow.BatchTerminate, MaxItems: 10, MaxConcurrency: 1,
		},
		workflow.PortSchema{},
	)

	baseRegistry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	expandedRegistry := newCheckpointExpandedRegistry(t, action)
	basePlan := compileFingerprintPlan(t, definition, baseRegistry)

	expandedPlan := compileFingerprintPlan(t, definition, expandedRegistry)
	if basePlan.Fingerprint() != expandedPlan.Fingerprint() {
		t.Fatal("unused contracts changed Batch Plan fingerprint")
	}

	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v3-batch")
	interrupted, err := runner.Run(t.Context(), basePlan, map[string]workflow.Value{
		"items": workflow.MustValueOf([]string{"item"}),
	})
	assertInterrupted(t, interrupted, err, "checkpoint-v3-batch")
	context := interrupted.Interruption.Contexts[0]

	resumed, err := runner.Resume(
		t.Context(),
		expandedPlan,
		"checkpoint-v3-batch",
		[]workflow.ResumeTarget{{InterruptID: context.ID}},
	)
	if err != nil {
		t.Fatalf("Runner.Resume() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 2 {
		t.Fatalf("resumed status = %q, calls = %d", resumed.Status, calls.Load())
	}
}

func TestCheckpointV3ResumesLoopAfterUnusedContracts(t *testing.T) {
	t.Parallel()

	integerSchema := mustSchema(t, `{"type":"integer"}`)
	countSchema := mustSchema(t, `{"type":"integer","minimum":1}`)

	var calls atomic.Int32

	action := &fakeAction{
		spec: actionSpec(
			"checkpoint_loop_scoped",
			map[string]workflow.PortSchema{"index": integerSchema},
			map[string]workflow.PortSchema{},
		),
		run: func(ctx context.Context, input workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			isTarget, _, _ := workflow.GetResumeContext(ctx)
			if !isTarget {
				return workflow.ActionOutput{}, workflow.Interrupt(ctx, input.Values["index"])
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{}}, nil
		},
	}
	body := workflow.Definition{
		Schema:   workflow.SchemaV1Alpha1,
		ID:       "checkpoint-loop-body",
		Revision: "v1",
		Name:     "Checkpoint Loop Body",
		Inputs:   map[string]workflow.PortSchema{"index": integerSchema},
		Outputs:  map[string]workflow.OutputBinding{},
		Nodes: []workflow.NodeDefinition{
			{ID: "start", Type: workflow.NodeTypeStart, Version: workflow.BuiltinNodeVersion},
			{
				ID: "gate", Type: workflow.NodeTypeAction, Version: workflow.BuiltinNodeVersion,
				Config: actionConfig(t, "checkpoint_loop_scoped"),
				Inputs: map[string]workflow.Binding{"index": workflowInput("index")},
			},
			{ID: "end", Type: workflow.NodeTypeEnd, Version: workflow.BuiltinNodeVersion},
		},
		Edges: []workflow.ControlEdge{
			edge("start", workflow.RouteSuccess, "gate"),
			edge("gate", workflow.RouteSuccess, "end"),
		},
		Limits: workflow.DefaultLimits(),
	}
	definition := loopParentDefinition(
		t,
		"checkpoint-scoped-loop",
		map[string]workflow.PortSchema{"count": countSchema},
		map[string]workflow.Binding{"count": workflowInput("count")},
		map[string]workflow.OutputBinding{},
		workflow.LoopConfig{
			Body: body, Mode: workflow.LoopCount, MaxIterations: 10,
		},
	)

	baseRegistry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	expandedRegistry := newCheckpointExpandedRegistry(t, action)
	basePlan := compileFingerprintPlan(t, definition, baseRegistry)

	expandedPlan := compileFingerprintPlan(t, definition, expandedRegistry)
	if basePlan.Fingerprint() != expandedPlan.Fingerprint() {
		t.Fatal("unused contracts changed Loop Plan fingerprint")
	}

	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v3-loop")
	interrupted, err := runner.Run(t.Context(), basePlan, map[string]workflow.Value{
		"count": workflow.MustValueOf(1),
	})
	assertInterrupted(t, interrupted, err, "checkpoint-v3-loop")
	context := interrupted.Interruption.Contexts[0]

	resumed, err := runner.Resume(
		t.Context(),
		expandedPlan,
		"checkpoint-v3-loop",
		[]workflow.ResumeTarget{{InterruptID: context.ID}},
	)
	if err != nil {
		t.Fatalf("Runner.Resume() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 2 {
		t.Fatalf("resumed status = %q, calls = %d", resumed.Status, calls.Load())
	}
}

func TestCheckpointV3ResumesNodeDebugAfterUnusedContracts(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := resultAction(
		"checkpoint_debug_scoped",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			calls.Add(1)

			return workflow.MustValueOf("debug"), nil
		},
	)
	definition := singleActionDefinition(
		t,
		"checkpoint_debug_scoped",
		stringSchema,
		workflow.NodePolicy{},
	)

	baseRegistry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	basePlan := compileCheckpointIdentityPlan(t, definition, baseRegistry)
	expandedPlan := compileCheckpointIdentityPlan(
		t,
		definition,
		newCheckpointExpandedRegistry(t, action),
	)

	baseDebug, err := workflow.PrepareNodeDebug(basePlan, workflow.NewNodePath("action"))
	if err != nil {
		t.Fatalf("PrepareNodeDebug(base) error = %v", err)
	}

	expandedDebug, err := workflow.PrepareNodeDebug(
		expandedPlan,
		workflow.NewNodePath("action"),
	)
	if err != nil {
		t.Fatalf("PrepareNodeDebug(expanded) error = %v", err)
	}

	if baseDebug.Fingerprint() != expandedDebug.Fingerprint() {
		t.Fatal("unused contracts changed Node Debug fingerprint")
	}

	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v3-debug")

	interrupted, err := runner.DebugNode(t.Context(), baseDebug, map[string]workflow.Value{})
	if !errors.Is(err, workflow.ErrInterrupted) || interrupted.Status != workflow.RunStatusInterrupted {
		t.Fatalf("DebugNode() result = %#v, error = %v", interrupted, err)
	}

	resumed, err := runner.ResumeNodeDebug(
		t.Context(),
		expandedDebug,
		"checkpoint-v3-debug",
		nil,
	)
	if err != nil {
		t.Fatalf("ResumeNodeDebug() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("resumed status = %q, calls = %d", resumed.Status, calls.Load())
	}
}

func TestCheckpointV3ResumesPartialRunAfterUnusedContracts(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := resultAction(
		"checkpoint_partial_scoped",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			calls.Add(1)

			return workflow.MustValueOf("partial"), nil
		},
	)
	definition := singleActionDefinition(
		t,
		"checkpoint_partial_scoped",
		stringSchema,
		workflow.NodePolicy{},
	)

	baseRegistry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	basePlan := compileCheckpointIdentityPlan(t, definition, baseRegistry)
	expandedPlan := compileCheckpointIdentityPlan(
		t,
		definition,
		newCheckpointExpandedRegistry(t, action),
	)
	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, "checkpoint-v3-partial")
	interrupted, err := runner.RunPartial(
		t.Context(),
		basePlan,
		"action",
		workflow.PartialRunInput{Inputs: map[string]workflow.Value{}},
	)
	assertPartialInterrupted(t, interrupted, err, "checkpoint-v3-partial")

	resumed, err := runner.ResumePartial(
		t.Context(),
		expandedPlan,
		"action",
		"checkpoint-v3-partial",
		nil,
	)
	if err != nil {
		t.Fatalf("ResumePartial() error = %v", err)
	}

	if resumed.Status != workflow.RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("resumed status = %q, calls = %d", resumed.Status, calls.Load())
	}
}

func newCheckpointExpandedRegistry(
	t *testing.T,
	usedAction workflow.Action,
) *workflow.Registry {
	t.Helper()

	unusedAction := &fakeAction{spec: actionSpec(
		"checkpoint_unused",
		map[string]workflow.PortSchema{},
		map[string]workflow.PortSchema{},
	)}

	nodeTypes := append(workflow.BuiltinNodeTypes(), fingerprintNodeType{
		key: "checkpoint_unused",
	})

	registry, err := workflow.NewRegistry(
		nodeTypes,
		[]workflow.Action{usedAction, unusedAction},
	)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	return registry
}

func compileCheckpointIdentityPlan(
	t *testing.T,
	definition workflow.Definition,
	registry *workflow.Registry,
) *workflow.Plan {
	t.Helper()

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("action")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	return plan
}

func compileCustomCheckpointPlan(
	t *testing.T,
	definition workflow.Definition,
	registry *workflow.Registry,
) *workflow.Plan {
	t.Helper()

	plan, err := workflow.Compile(
		t.Context(),
		definition,
		registry,
		workflow.WithInterruptBeforeNodes(workflow.NewNodePath("custom")),
	)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	return plan
}

func assertCheckpointIdentityRejected(
	t *testing.T,
	writePlan *workflow.Plan,
	resumePlan *workflow.Plan,
	calls *atomic.Int32,
	runID string,
) {
	t.Helper()

	if writePlan.Fingerprint() == resumePlan.Fingerprint() {
		t.Fatal("observed contract change did not change Plan fingerprint")
	}

	store := &memoryCheckpointStore{}
	runner := checkpointIdentityRunner(t, store, runID)
	interrupted, err := runner.Run(t.Context(), writePlan, map[string]workflow.Value{})
	assertInterrupted(t, interrupted, err, runID)

	_, err = runner.Resume(t.Context(), resumePlan, runID, nil)
	if !errors.Is(err, workflow.ErrRun) || errors.Is(err, workflow.ErrInterrupted) {
		t.Fatalf("Runner.Resume() error = %v, want ErrRun only", err)
	}

	if calls.Load() != 0 {
		t.Fatalf("node calls after identity rejection = %d, want 0", calls.Load())
	}
}

func checkpointIdentityRunner(
	t *testing.T,
	store workflow.CheckpointStore,
	runID string,
) *workflow.Runner {
	t.Helper()

	runner, err := workflow.NewRunner(
		workflow.WithCheckpointStore(store),
		workflow.WithRunIDSource(func(time.Time) (string, error) { return runID, nil }),
	)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}

	return runner
}

func assertCurrentCheckpointHeader(
	t *testing.T,
	data []byte,
	plan *workflow.Plan,
) {
	t.Helper()

	var checkpoint map[string]any
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		t.Fatalf("json.Unmarshal(checkpoint) error = %v", err)
	}

	contract, ok := checkpoint["contract_fingerprint"].(string)
	if checkpoint["version"] != float64(3) || !ok || len(contract) != 64 {
		t.Fatalf("checkpoint current identity header = %#v", checkpoint)
	}

	if _, exists := checkpoint["registry_fingerprint"]; exists {
		t.Fatal("checkpoint v3 contains registry_fingerprint")
	}

	if checkpoint["plan_fingerprint"] != plan.Fingerprint() {
		t.Fatalf("checkpoint plan fingerprint = %v, want %q", checkpoint["plan_fingerprint"], plan.Fingerprint())
	}

	execution := checkpointExecution(t, checkpoint)
	if execution["plan_fingerprint"] != plan.Fingerprint() {
		t.Fatalf("execution plan fingerprint = %v, want %q", execution["plan_fingerprint"], plan.Fingerprint())
	}
}

func legacyGoldenCheckpointPlan(
	t *testing.T,
) (*workflow.Plan, workflow.Action, *atomic.Int32) {
	t.Helper()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	calls := &atomic.Int32{}
	action := resultAction(
		"legacy_golden_action",
		stringSchema,
		func(context.Context) (workflow.Value, error) {
			calls.Add(1)

			return workflow.MustValueOf("ok"), nil
		},
	)

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	if registry.Fingerprint() != legacyGoldenRegistryFingerprint {
		t.Fatalf(
			"Registry.Fingerprint() = %q, want legacy golden %q",
			registry.Fingerprint(),
			legacyGoldenRegistryFingerprint,
		)
	}

	definition := legacyGoldenDefinition(t)

	return compileCheckpointIdentityPlan(t, definition, registry), action, calls
}

func legacyGoldenDefinition(t *testing.T) workflow.Definition {
	t.Helper()

	return singleActionDefinition(
		t,
		"legacy_golden_action",
		mustSchema(t, `{"type":"string"}`),
		workflow.NodePolicy{},
	)
}

func legacyGoldenNodeDebugFingerprint(
	t *testing.T,
	source *workflow.Plan,
	debugPlan *workflow.NodeDebugPlan,
	node workflow.NodeDefinition,
) string {
	t.Helper()

	spec := debugPlan.Spec()
	definitionFingerprint := legacyFingerprintValue(t, struct {
		Strategy         string                         `json:"strategy"`
		SourceDefinition string                         `json:"source_definition"`
		Target           []workflow.NodeID              `json:"target"`
		Node             workflow.NodeDefinition        `json:"node"`
		Inputs           map[string]workflow.PortSchema `json:"inputs"`
		Outputs          map[string]workflow.PortSchema `json:"outputs"`
		Routes           []string                       `json:"routes"`
	}{
		Strategy:         "pips.workflow/node-debug-definition/v1",
		SourceDefinition: source.DefinitionFingerprint(),
		Target:           []workflow.NodeID{"action"},
		Node:             node,
		Inputs:           spec.Inputs,
		Outputs:          spec.Outputs,
		Routes:           spec.Routes,
	})

	return legacyFingerprintValue(t, struct {
		Strategy        string   `json:"strategy"`
		Definition      string   `json:"definition"`
		Registry        string   `json:"registry"`
		SourcePlan      string   `json:"source_plan"`
		OptionalInputs  []string `json:"optional_inputs"`
		InterruptBefore bool     `json:"interrupt_before"`
		InterruptAfter  bool     `json:"interrupt_after"`
	}{
		Strategy:        "pips.workflow/node-debug-plan/v1",
		Definition:      definitionFingerprint,
		Registry:        legacyGoldenRegistryFingerprint,
		SourcePlan:      legacyGoldenRootPlanFingerprint,
		OptionalInputs:  spec.OptionalInputs,
		InterruptBefore: true,
		InterruptAfter:  false,
	})
}

func legacyFingerprintValue(t *testing.T, value any) string {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(legacy fingerprint) error = %v", err)
	}

	digest := sha256.Sum256(data)

	return hex.EncodeToString(digest[:])
}

func asLegacyGoldenCheckpoint(t *testing.T, data []byte) []byte {
	t.Helper()

	return mutateCheckpointJSON(t, data, func(checkpoint map[string]any) {
		checkpoint["version"] = float64(2)
		delete(checkpoint, "contract_fingerprint")
		checkpoint["registry_fingerprint"] = legacyGoldenRegistryFingerprint
		checkpoint["plan_fingerprint"] = legacyGoldenRootPlanFingerprint
		execution := checkpointExecution(t, checkpoint)
		execution["plan_fingerprint"] = legacyGoldenRootPlanFingerprint
	})
}

func nestedCheckpointIdentityPlan(
	t *testing.T,
	expanded bool,
) (*workflow.Plan, workflow.Action, *atomic.Int32) {
	t.Helper()

	calls := &atomic.Int32{}
	stringSchema := mustSchema(t, `{"type":"string"}`)
	action := &fakeAction{
		spec: actionSpec(
			"legacy_child_action",
			map[string]workflow.PortSchema{"value": stringSchema},
			map[string]workflow.PortSchema{"result": stringSchema},
		),
		run: func(ctx context.Context, _ workflow.ActionInput) (workflow.ActionOutput, error) {
			calls.Add(1)

			isTarget, hasData, data := workflow.GetResumeContext(ctx)
			if !isTarget {
				return workflow.ActionOutput{}, workflow.Interrupt(
					ctx,
					workflow.MustValueOf("nested"),
				)
			}

			if !hasData {
				return workflow.ActionOutput{}, errors.New("missing nested resume data")
			}

			return workflow.ActionOutput{Values: map[string]workflow.Value{
				"result": data,
			}}, nil
		},
	}

	plan, _, _ := nestedCheckpointIdentityPlanWithAction(t, action, expanded)

	return plan, action, calls
}

func nestedCheckpointIdentityPlanWithAction(
	t *testing.T,
	action workflow.Action,
	expanded bool,
) (*workflow.Plan, workflow.Definition, *workflow.Registry) {
	t.Helper()

	stringSchema := mustSchema(t, `{"type":"string"}`)
	child := subWorkflowChildDefinition(stringSchema, "legacy_child_action")
	parent := subWorkflowParentDefinition(t, stringSchema, workflowRef(t, child))

	registry, err := workflow.NewDefaultRegistry(action)
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}

	if expanded {
		registry = newCheckpointExpandedRegistry(t, action)
	}

	plan, err := workflow.Compile(
		t.Context(),
		parent,
		registry,
		workflow.WithDefinitionResolver(staticResolver(child)),
	)
	if err != nil {
		t.Fatalf("Compile(nested) error = %v", err)
	}

	return plan, parent, registry
}

func checkpointPausedChild(t *testing.T, execution map[string]any) map[string]any {
	t.Helper()

	paused, ok := execution["paused"].([]any)
	if !ok || len(paused) != 1 {
		t.Fatalf("checkpoint paused state = %#v", execution["paused"])
	}

	node, ok := paused[0].(map[string]any)
	if !ok {
		t.Fatal("checkpoint paused node is not an object")
	}

	child, ok := node["child"].(map[string]any)
	if !ok {
		t.Fatal("checkpoint paused child is not an object")
	}

	return child
}
