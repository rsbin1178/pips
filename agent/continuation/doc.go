// Package continuation provides durable, application-driven execution across bounded
// worker runs. It owns lifecycle, accounting, wakeups, and retry boundaries;
// optional completion and activation policies live in the sibling goal and
// loop packages. Team, workflow, and product scheduling remain above it.
//
// The package never starts a scheduler or background retry loop. Applications call
// Advance for one attempt or Drive for a bounded synchronous sequence, and
// explicitly deliver time and signal wakeups.
package continuation
