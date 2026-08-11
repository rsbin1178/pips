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
//
// Runs can stop at compile-time before/after boundaries, at dynamic
// Interrupt calls, or on a host signal. A CheckpointStore persists one opaque
// snapshot per Run ID so a new Runner can validate the same Plan and Resume
// the exact root and composite frontier. Checkpoints are process-runtime state,
// not part of the Definition wire contract.
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
// SchemaV1Alpha1 is an alpha wire contract: decoders reject unknown schema
// versions and fields instead of attempting implicit migration. Events are
// process-local observations and intentionally have no JSON wire format.
package workflow
