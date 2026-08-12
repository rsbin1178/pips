// Package workflow compiles versioned, declarative workflow definitions into
// immutable in-process execution plans.
//
// Workflow is independent of the ai and agent packages. Applications provide
// behavior through explicitly registered Actions; definitions contain data,
// bindings, and control edges, never executable code.
//
// Built-in composition remains fixed-flow and synchronous: Selector chooses
// one ordered route, SubWorkflow executes an exact resolved revision, Batch
// maps an inline child Definition over an array with bounded workers, and Loop
// sequentially executes a bounded inline body with transactional local
// variables. Batch and Loop are distinct and cannot contain each other.
// Merge v1 preserves exactly-one-present branch aggregation; Merge v2 uses
// ordered first-non-null selection for route-relevant upstream candidates.
// Both versions keep parallel Merge as direct wait-all fan-in.
//
// Runs can stop at compile-time before/after boundaries, at dynamic
// Interrupt calls, or on a host signal. A CheckpointStore persists one opaque
// snapshot per Run ID so a new Runner can validate the same Plan and Resume
// the exact root and composite frontier. Checkpoints are process-runtime state,
// not part of the Definition wire contract.
//
// During a real node invocation, Actions and custom NodeTypes can call
// GetExecutionContext to read the current Run, Definition, scoped node address,
// and cumulative one-based attempt. The returned address is detached runtime
// correlation metadata; hosts remain responsible for defining idempotency at
// their own side-effect boundaries.
//
// Exhausted node failures either stop execution, select a dedicated error
// route, or continue with compile-validated default outputs. Error-route
// consumers use BindingNodeError to read the bounded error_message and
// error_type ports; normal outputs remain unavailable on that path. Handled
// nodes have NodeStatusException, and a completed root execution that observed
// one has RunStatusPartialSucceeded. Error messages are sensitive Workflow
// data and never lifecycle Event payloads.
//
// RunPartial executes the root dependency slice ending at one destination.
// Hosts may supply detached successful data from a prior Partial Run, current
// pinned outputs, and directly changed node IDs; the runtime validates routes
// and schemas, invalidates dirty descendants, and executes every remaining
// boundary through the ordinary scheduler. Composite nodes remain atomic.
// ResumePartial uses the same opaque CheckpointStore protocol, while Run and
// Resume never consult Partial Run data.
//
// PrepareNodeDebug derives a Coze-style isolated trial plan for one eligible
// node. DebugNode executes that selected node without executing its upstream
// graph; outside bindings become explicit final-schema inputs while literals
// and selected composite child Plans remain internal. Results contain
// sensitive resolved values and Action error text, so hosts own authorization,
// redaction, encryption, and retention. ResumeNodeDebug uses the same opaque
// CheckpointStore boundary as Resume. Node Debug invokes real Actions and is
// distinct from partial graph replay or pinned-data execution.
//
// SchemaV1Alpha1 preserves required schema-only Workflow inputs. V1Alpha2 adds
// required, optional, and non-null default input contracts while retaining
// strict version-directed decoding. Defaults normalize only at a Workflow
// boundary, never as generic Action or NodeType port behavior. Events are
// process-local observations and intentionally have no JSON wire format.
//
// Compile reports Definition diagnostics as a *CompileError containing stable
// issue codes and Definition, node, or control-path locations. Diagnostics are
// phase-gated: Compile returns all safely independent issues in the earliest
// failed compiler phase, not every hypothetical downstream problem. Nested
// SubWorkflow, Batch, and Loop issues retain full NodePath locations. Issues
// are process-local values with no prescribed JSON or persistence format;
// hosts own localization and versioned API, database, or editor projections.
package workflow
