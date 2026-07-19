// Package team provides durable, host-driven coordination for a flat team of
// independent Agent sessions. It owns members, dependent tasks, exclusive
// assignment and claims, task attempts, direct mailboxes, and Team lifecycle.
//
// Team never starts Agents, schedulers, timers, or background goroutines. An
// embedding host runtime registers member resources, starts a task attempt,
// drives the referenced continuation execution, and explicitly commits the
// attempt result. Goal and Loop may control an individual child continuation;
// Workflow and distributed scheduling remain application layers above Team.
package team
