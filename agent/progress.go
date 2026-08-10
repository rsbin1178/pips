package agent

import (
	"context"

	"github.com/rsbin/pips/ai"
)

// progressKey carries the per-call progress reporter through the tool's
// context.
type progressKey struct{}

type progressFunc func(parts []ai.Part)

// ReportProgress publishes a partial-result update from inside a running
// tool's Exec, surfacing as an [EventToolUpdated] event to the run's event
// consumers. Updates are advisory: delivery is best-effort (slow consumers
// drop updates rather than block the tool), and calls outside a run are
// no-ops.
func ReportProgress(ctx context.Context, parts ...ai.Part) {
	report, ok := ctx.Value(progressKey{}).(progressFunc)
	if !ok || len(parts) == 0 {
		return
	}

	report(parts)
}

// withProgress derives a tool context carrying a reporter.
func withProgress(ctx context.Context, report progressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}
