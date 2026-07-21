// Package otel adapts content-free Coding events to OpenTelemetry traces and
// metrics. Applications own SDK, exporter, processing, and provider shutdown.
//
//nolint:wsl_v5 // OTel instrument construction and event mapping stay grouped by signal.
package otel

import (
	"context"
	"fmt"
	"time"

	"github.com/rsbin/pips/internal/coding"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/rsbin/pips/internal/coding/observability/otel"

// Config supplies providers owned by the application. Nil providers use the
// OpenTelemetry global providers.
type Config struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

// Observer maps product lifecycle telemetry into OTel. It owns no exporter,
// provider, goroutine, or cross-event state and is safe to share across
// multiple Coding runtimes.
type Observer struct {
	tracer trace.Tracer

	sessions            metric.Int64Counter
	interactions        metric.Int64Counter
	interactionDuration metric.Int64Histogram
	approvals           metric.Int64Counter
	workspaceChanges    metric.Int64Counter
	diagnostics         metric.Int64Counter
}

// New constructs an observer using application-owned or global providers.
func New(config Config) (*Observer, error) {
	tp, mp := config.TracerProvider, config.MeterProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	if mp == nil {
		mp = otel.GetMeterProvider()
	}

	meter := mp.Meter(instrumentationName)
	sessions, err := meter.Int64Counter(
		"pips.coding.sessions",
		metric.WithDescription("Coding session lifecycle events."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel sessions counter: %w", err)
	}

	interactions, err := meter.Int64Counter(
		"pips.coding.interactions",
		metric.WithDescription("Completed Coding interactions."),
		metric.WithUnit("{interaction}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel interactions counter: %w", err)
	}

	duration, err := meter.Int64Histogram(
		"pips.coding.interaction.duration",
		metric.WithDescription("Coding interaction duration."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel interaction duration: %w", err)
	}

	approvals, err := meter.Int64Counter(
		"pips.coding.approvals",
		metric.WithDescription("Coding approval lifecycle events."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel approvals counter: %w", err)
	}

	workspaceChanges, err := meter.Int64Counter(
		"pips.coding.workspace.changes",
		metric.WithDescription("Workspace paths changed by Coding interactions."),
		metric.WithUnit("{change}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel workspace changes counter: %w", err)
	}

	diagnostics, err := meter.Int64Counter(
		"pips.coding.integration.diagnostics",
		metric.WithDescription("Coding integration diagnostics."),
		metric.WithUnit("{diagnostic}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel diagnostics counter: %w", err)
	}

	return &Observer{
		tracer:              tp.Tracer(instrumentationName),
		sessions:            sessions,
		interactions:        interactions,
		interactionDuration: duration,
		approvals:           approvals,
		workspaceChanges:    workspaceChanges,
		diagnostics:         diagnostics,
	}, nil
}

// Observe translates one content-free product event. Calls are synchronous;
// applications should use an OTel batch processor when export may block.
func (o *Observer) Observe(ctx context.Context, event coding.TelemetryEvent) error {
	if o == nil {
		return nil
	}

	switch event.Type {
	case coding.EventSessionOpened, coding.EventSessionClosed:
		o.observeSession(ctx, event)
	case coding.EventInteractionCompleted:
		o.observeInteraction(ctx, event)
	case coding.EventApprovalRequired, coding.EventApprovalUnknown, coding.EventApprovalResolved:
		o.observeApproval(ctx, event)
	case coding.EventWorkspaceChanged:
		attrs := []attribute.KeyValue{attribute.Int("coding.workspace.changes", event.Changes)}
		o.workspaceChanges.Add(ctx, int64(event.Changes))
		o.instant(ctx, "coding.workspace.changed", event, attrs, false)
	case coding.EventIntegrationDiagnostic:
		attrs := []attribute.KeyValue{
			attribute.String("coding.integration.component", event.Component),
			attribute.String("coding.integration.code", event.Code),
		}
		o.diagnostics.Add(ctx, 1, metric.WithAttributes(attrs...))
		o.instant(ctx, "coding.integration.diagnostic", event, attrs, true)
	case coding.EventError:
		o.instant(ctx, "coding.error", event, []attribute.KeyValue{
			attribute.String("coding.error.code", event.Code),
		}, true)
	case coding.EventInteractionStarted, coding.EventRunStarted, coding.EventRunCompleted,
		coding.EventTurnStarted, coding.EventTurnCompleted, coding.EventMessageCommitted,
		coding.EventMessageDelta, coding.EventToolStarted, coding.EventToolUpdated,
		coding.EventToolCompleted, coding.EventStatusChanged:
		// Raw Agent observers cover run/turn/tool signals. Status and content
		// events intentionally create no duplicate product metrics.
	}

	return nil
}

func (o *Observer) observeSession(ctx context.Context, event coding.TelemetryEvent) {
	lifecycle := "opened"
	attrs := []attribute.KeyValue{
		attribute.String("coding.session.lifecycle", lifecycle),
		attribute.String("coding.provider", string(event.Provider)),
		attribute.String("coding.model", event.ModelID),
		attribute.Bool("coding.session.resumed", event.Resumed),
	}
	name := "coding.session.opened"
	if event.Type == coding.EventSessionClosed {
		lifecycle = "closed"
		attrs = []attribute.KeyValue{
			attribute.String("coding.session.lifecycle", lifecycle),
			attribute.String("coding.session.reason", event.Code),
		}
		name = "coding.session.closed"
	}

	o.sessions.Add(ctx, 1, metric.WithAttributes(attrs...))
	o.instant(ctx, name, event, attrs, false)
}

func (o *Observer) observeInteraction(ctx context.Context, event coding.TelemetryEvent) {
	attrs := []attribute.KeyValue{
		attribute.String("coding.interaction.outcome", string(event.Outcome)),
		attribute.Int("coding.usage.input_tokens", event.Usage.InputTokens),
		attribute.Int("coding.usage.output_tokens", event.Usage.OutputTokens),
		attribute.Int("coding.usage.reasoning_tokens", event.Usage.ReasoningTokens),
		attribute.Int("coding.usage.cached_input_tokens", event.Usage.CachedInputTokens),
		attribute.Int("coding.usage.cache_write_tokens", event.Usage.CacheWriteTokens),
	}
	metricAttrs := []attribute.KeyValue{
		attribute.String("coding.interaction.outcome", string(event.Outcome)),
	}
	o.interactions.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
	o.interactionDuration.Record(
		ctx,
		event.DurationMillis,
		metric.WithAttributes(metricAttrs...),
	)

	end := eventTime(event)
	start := end.Add(-time.Duration(event.DurationMillis) * time.Millisecond)
	_, span := o.tracer.Start(
		ctx,
		"coding.interaction",
		trace.WithTimestamp(start),
		trace.WithAttributes(attrs...),
	)
	if event.Failed {
		span.SetStatus(codes.Error, "interaction failed")
	}
	span.End(trace.WithTimestamp(end))
}

func (o *Observer) observeApproval(ctx context.Context, event coding.TelemetryEvent) {
	state := "required"
	failed := false
	attrs := []attribute.KeyValue{attribute.String("coding.tool.name", event.Tool)}
	switch event.Type {
	case coding.EventApprovalUnknown:
		state = "unknown"
		failed = true
	case coding.EventApprovalResolved:
		state = "resolved"
		attrs = append(attrs, attribute.String("coding.approval.choice", event.Code))
	case coding.EventApprovalRequired:
	default:
		return
	}
	attrs = append(attrs, attribute.String("coding.approval.state", state))
	o.approvals.Add(ctx, 1, metric.WithAttributes(attrs...))
	o.instant(ctx, "coding.approval."+state, event, attrs, failed)
}

func (o *Observer) instant(
	ctx context.Context,
	name string,
	event coding.TelemetryEvent,
	attrs []attribute.KeyValue,
	failed bool,
) {
	at := eventTime(event)
	_, span := o.tracer.Start(
		ctx,
		name,
		trace.WithTimestamp(at),
		trace.WithAttributes(attrs...),
	)
	if failed {
		span.SetStatus(codes.Error, "coding lifecycle event failed")
	}
	span.End(trace.WithTimestamp(at))
}

func eventTime(event coding.TelemetryEvent) time.Time {
	if event.Time.IsZero() {
		return time.Now()
	}

	return event.Time
}

var _ coding.TelemetryObserver = (*Observer)(nil)
