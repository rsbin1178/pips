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
// SchemaV1Alpha1 is an alpha wire contract: decoders reject unknown schema
// versions and fields instead of attempting implicit migration. Events are
// process-local observations and intentionally have no JSON wire format.
package workflow
