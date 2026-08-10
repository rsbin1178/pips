// Package otel adapts pips agent events to OpenTelemetry traces and metrics.
// Applications own SDK, exporter, sampling, and resource setup; this package
// accepts their providers and emits no prompt, tool arguments, or tool output.
package otel

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/rsbin/pips/agent"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/rsbin/pips/agent/observability/otel"

// Config supplies providers owned by the application. Nil providers use the
// OpenTelemetry global providers.
type Config struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

// Observer maps agent lifecycle events into OTel spans and counters. It is
// safe for concurrent runs and suitable for agent.WithOnEvent.
type Observer struct {
	tracer trace.Tracer
	runs   sync.Map // map[string]trace.Span
	turns  sync.Map // map[turnKey]trace.Span
	tools  sync.Map // map[toolKey]trace.Span

	runsTotal  metric.Int64Counter
	toolsTotal metric.Int64Counter
	failures   metric.Int64Counter
	tokens     metric.Int64Counter
}

// New constructs an OTel observer using application-owned providers.
func New(config Config) (*Observer, error) {
	tp, mp := config.TracerProvider, config.MeterProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}

	if mp == nil {
		mp = otel.GetMeterProvider()
	}

	meter := mp.Meter(instrumentationName)

	runs, err := meter.Int64Counter("pips.agent.runs")
	if err != nil {
		return nil, fmt.Errorf("otel runs counter: %w", err)
	}

	tools, err := meter.Int64Counter("pips.agent.tool_calls")
	if err != nil {
		return nil, fmt.Errorf("otel tool counter: %w", err)
	}

	failures, err := meter.Int64Counter("pips.agent.tool_failures")
	if err != nil {
		return nil, fmt.Errorf("otel failures counter: %w", err)
	}

	tokens, err := meter.Int64Counter("pips.agent.tokens")
	if err != nil {
		return nil, fmt.Errorf("otel tokens counter: %w", err)
	}

	return &Observer{tracer: tp.Tracer(instrumentationName), runsTotal: runs, toolsTotal: tools, failures: failures, tokens: tokens}, nil
}

// Observe translates an event. Agent calls this synchronously; exporters must
// therefore be configured for non-blocking export by the application.
//
//nolint:gocyclo // Each event variant has an intentionally distinct OTel mapping.
func (o *Observer) Observe(ctx context.Context, event agent.Event) {
	if o == nil || event.RunID == "" {
		return
	}

	attrs := runAttributes(event, 0)
	switch payload := event.Payload().(type) {
	case agent.RunStarted:
		_, span := o.tracer.Start(ctx, "agent.run", trace.WithAttributes(attrs...))
		o.runs.Store(event.RunID, span)
		o.runsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	case agent.TurnStarted:
		o.startChild(ctx, event, payload.Turn, "agent.turn", &o.turns, turnKey(event.RunID, payload.Turn))
	case agent.TurnCompleted:
		attrs = runAttributes(event, payload.Turn)
		o.end(&o.turns, turnKey(event.RunID, payload.Turn), event, false, "")
		o.tokens.Add(ctx, int64(payload.Usage.InputTokens+payload.Usage.OutputTokens), metric.WithAttributes(attrs...))
	case agent.ToolStarted:
		o.startChild(
			ctx,
			event,
			payload.Turn,
			"agent.tool",
			&o.tools,
			toolKey(event.RunID, payload.Call.ID),
			attribute.String("agent.tool.name", payload.Call.Name),
		)
	case agent.ToolCompleted:
		attrs = runAttributes(event, payload.Turn)
		o.end(&o.tools, toolKey(event.RunID, payload.Call.ID), event, payload.Result.IsError, "")
		o.toolsTotal.Add(ctx, 1, metric.WithAttributes(append(attrs, attribute.String("agent.tool.name", payload.Call.Name))...))

		if payload.Result.IsError {
			o.failures.Add(ctx, 1, metric.WithAttributes(attrs...))
		}
	case agent.RunCompleted:
		o.end(&o.runs, event.RunID, event, false, payload.Stop)
	case agent.ModelStreamEvent, agent.MessageCommitted, agent.CandidateDiscarded,
		agent.ToolUpdated:
		// These events do not change span lifecycle or aggregate metrics.
	}
}

func (o *Observer) startChild(ctx context.Context, event agent.Event, turn int, name string, target *sync.Map, key string, extra ...attribute.KeyValue) {
	if parent, ok := o.runs.Load(event.RunID); ok {
		if span, isSpan := parent.(trace.Span); isSpan {
			ctx = trace.ContextWithSpan(ctx, span)
		}
	}

	attrs := append(runAttributes(event, turn), extra...)
	_, span := o.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	target.Store(key, span)
}

func (o *Observer) end(source *sync.Map, key string, event agent.Event, failed bool, stop agent.StopReason) {
	if value, ok := source.LoadAndDelete(key); ok {
		span, isSpan := value.(trace.Span)
		if !isSpan {
			return
		}

		span.SetAttributes(attribute.String("agent.stop_reason", string(stop)))

		if failed {
			span.SetStatus(codes.Error, "tool failed")
		}

		span.End(trace.WithTimestamp(event.Time))
	}
}

func runAttributes(event agent.Event, turn int) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String("agent.run_id", event.RunID), attribute.String("agent.parent_run_id", event.ParentRunID), attribute.String("agent.name", event.Agent), attribute.Int("agent.turn", turn)}
}
func turnKey(runID string, turn int) string { return runID + ":" + strconv.Itoa(turn) }
func toolKey(runID, callID string) string   { return runID + ":" + callID }
