// Package loop provides fixed and dynamic activation policies for continuation
// executions.
//
// A Controller plans one durable wait after each completed Work result. The
// package never sleeps, polls, starts a timer, or resumes an execution; applications
// explicitly deliver due-time or signal activations through continuation.
package loop
