// Package goal provides evidence-based completion policies for continuation
// executions.
//
// A Controller evaluates one durable Work result and decides whether the same
// execution should continue, complete, or block. The package owns no
// scheduler, lifecycle store, conversation, or background goroutine.
package goal
