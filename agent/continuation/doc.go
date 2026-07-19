// Package continuation provides durable, host-driven execution across bounded
// worker runs. It owns lifecycle, accounting, wakeups, and retry boundaries;
// product goal, loop, team, and workflow policies remain controllers above it.
//
// The package never starts a scheduler or background retry loop. Hosts call
// Advance for one attempt or Drive for a bounded synchronous sequence, and
// explicitly deliver time and signal wakeups.
package continuation
