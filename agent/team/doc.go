// Package team provides durable coordination for a flat team of
// independent Agent sessions. It owns members, dependent tasks, exclusive
// assignment and claims, task attempts, direct mailboxes, and Team lifecycle.
//
// Team never starts Agents, schedulers, timers, or background goroutines. An
// application coordinator registers member resources. AttemptRuntime can
// synchronously compose the repeatable claim/start/mailbox/continuation/result
// lifecycle when an application supplies a Worker factory and result projector.
// It never owns models, Harness resources, routing, or scheduling. Goal and
// Loop may control an individual child continuation; Workflow and distributed
// scheduling remain application layers above Team.
package team
