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
	questions           metric.Int64Counter
	workspaceChanges    metric.Int64Counter
	diagnostics         metric.Int64Counter
	subagents           metric.Int64Counter
	subagentDuration    metric.Int64Histogram
	teams               metric.Int64Counter
	teamDuration        metric.Int64Histogram
	teamControls        metric.Int64Counter
	teamIntegrations    metric.Int64Counter
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
	questions, err := meter.Int64Counter(
		"pips.coding.questions",
		metric.WithDescription("Coding structured-question lifecycle events."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel questions counter: %w", err)
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

	subagents, err := meter.Int64Counter(
		"pips.coding.subagents",
		metric.WithDescription("Coding subagent lifecycle events."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel subagents counter: %w", err)
	}

	subagentDuration, err := meter.Int64Histogram(
		"pips.coding.subagent.duration",
		metric.WithDescription("Terminal Coding subagent duration."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel subagent duration: %w", err)
	}
	teams, err := meter.Int64Counter(
		"pips.coding.teams",
		metric.WithDescription("Coding Team lifecycle events."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel Teams counter: %w", err)
	}
	teamDuration, err := meter.Int64Histogram(
		"pips.coding.team.duration",
		metric.WithDescription("Terminal Coding Team Attempt duration."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel Team duration: %w", err)
	}
	teamControls, err := meter.Int64Counter(
		"pips.coding.team.controls",
		metric.WithDescription("Coding Team operator control lifecycle events."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel Team controls counter: %w", err)
	}
	teamIntegrations, err := meter.Int64Counter(
		"pips.coding.team.integrations",
		metric.WithDescription("Coding Team result integration lifecycle events."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("coding otel Team integrations counter: %w", err)
	}

	return &Observer{
		tracer:              tp.Tracer(instrumentationName),
		sessions:            sessions,
		interactions:        interactions,
		interactionDuration: duration,
		approvals:           approvals,
		questions:           questions,
		workspaceChanges:    workspaceChanges,
		diagnostics:         diagnostics,
		subagents:           subagents,
		subagentDuration:    subagentDuration,
		teams:               teams,
		teamDuration:        teamDuration,
		teamControls:        teamControls,
		teamIntegrations:    teamIntegrations,
	}, nil
}

// Observe translates one content-free product event. Calls are synchronous;
// applications should use an OTel batch processor when export may block.
//
//nolint:gocyclo // The event-family dispatch remains exhaustive and explicit.
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
	case coding.EventQuestionRequired, coding.EventQuestionResolved, coding.EventQuestionRejected:
		o.observeQuestion(ctx, event)
	case coding.EventModeChanged:
		o.instant(ctx, "coding.mode.changed", event, []attribute.KeyValue{
			attribute.String("coding.mode", string(event.Mode)),
		}, false)
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
	case coding.EventSessionTreeChanged:
		o.instant(ctx, "coding.session.tree.changed", event, []attribute.KeyValue{
			attribute.Int("coding.session.nodes", event.Nodes),
		}, false)
	case coding.EventSessionNavigated, coding.EventSessionForked:
		o.instant(ctx, "coding.session."+event.Code, event, nil, false)
	case coding.EventCompactionStarted, coding.EventCompactionCompleted:
		o.instant(ctx, "coding.compaction."+compactionLifecycle(event.Type), event, []attribute.KeyValue{
			attribute.String("coding.compaction.mode", string(event.CompactionMode)),
			attribute.Int("coding.compaction.tokens_before", event.TokensBefore),
			attribute.Int("coding.compaction.tokens_after", event.TokensAfter),
			attribute.Int64("coding.compaction.duration_ms", event.DurationMillis),
		}, event.Failed)
	case coding.EventSubagentCreated, coding.EventSubagentStarted,
		coding.EventSubagentProgress, coding.EventSubagentCompleted,
		coding.EventSubagentFailed, coding.EventSubagentCanceled,
		coding.EventSubagentInterrupted:
		o.observeSubagent(ctx, event)
	case coding.EventTeamLifecycle:
		o.observeTeam(ctx, event)
	case coding.EventTeamControlLifecycle:
		o.observeTeamControl(ctx, event)
	case coding.EventTeamIntegrationLifecycle:
		o.observeTeamIntegration(ctx, event)
	case coding.EventInteractionStarted, coding.EventRunStarted, coding.EventRunCompleted,
		coding.EventTurnStarted, coding.EventTurnCompleted, coding.EventMessageCommitted,
		coding.EventMessageDelta, coding.EventToolStarted, coding.EventToolUpdated,
		coding.EventToolCompleted, coding.EventStatusChanged:
		// Raw Agent observers cover run/turn/tool signals. Status and content
		// events intentionally create no duplicate product metrics.
	}

	return nil
}

func (o *Observer) observeTeamControl(ctx context.Context, event coding.TelemetryEvent) {
	attrs := []attribute.KeyValue{
		attribute.String("coding.team.control.action", event.TeamControlAction),
		attribute.String("coding.team.control.state", event.TeamControlState),
		attribute.String("coding.team.control.code", event.Code),
	}
	metricAttrs := attrs[:2]
	o.teamControls.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
	o.instant(ctx, "coding.team.control", event, attrs, event.Failed)
}

func (o *Observer) observeTeamIntegration(ctx context.Context, event coding.TelemetryEvent) {
	attrs := []attribute.KeyValue{
		attribute.String("coding.team.integration.state", event.IntegrationState),
		attribute.String("coding.team.integration.verification", event.IntegrationVerification),
		attribute.String("coding.team.integration.code", event.Code),
		attribute.Int("coding.team.integration.attempts", event.Attempts),
		attribute.Int("coding.team.integration.files", event.Files),
	}
	metricAttrs := []attribute.KeyValue{
		attribute.String("coding.team.integration.state", event.IntegrationState),
		attribute.String("coding.team.integration.verification", event.IntegrationVerification),
	}
	o.teamIntegrations.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
	o.instant(ctx, "coding.team.integration", event, attrs, event.Failed)
}

func (o *Observer) observeTeam(ctx context.Context, event coding.TelemetryEvent) {
	attrs := []attribute.KeyValue{
		attribute.String("coding.team.scope", event.TeamScope),
		attribute.String("coding.team.state", event.TeamState),
		attribute.String("coding.team.activity", event.TeamActivity),
		attribute.String("coding.team.code", event.Code),
		attribute.Int("coding.team.turns", event.Turns),
		attribute.Int("coding.team.tool_calls", event.ToolCalls),
		attribute.Int64("coding.team.duration_ms", event.DurationMillis),
		attribute.Int("coding.usage.input_tokens", event.Usage.InputTokens),
		attribute.Int("coding.usage.output_tokens", event.Usage.OutputTokens),
		attribute.Int("coding.usage.reasoning_tokens", event.Usage.ReasoningTokens),
	}
	metricAttrs := []attribute.KeyValue{
		attribute.String("coding.team.scope", event.TeamScope),
		attribute.String("coding.team.state", event.TeamState),
		attribute.String("coding.team.activity", event.TeamActivity),
	}
	o.teams.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
	if event.TeamScope == "attempt" && terminalTeamState(event.TeamState) {
		o.teamDuration.Record(
			ctx,
			event.DurationMillis,
			metric.WithAttributes(metricAttrs...),
		)
	}
	o.instant(ctx, "coding.team.lifecycle", event, attrs, event.Failed)
}

func terminalTeamState(state string) bool {
	return state == string(coding.TeamLifecycleCompleted) ||
		state == string(coding.TeamLifecycleFailed) ||
		state == string(coding.TeamLifecycleCancelled) ||
		state == string(coding.TeamLifecycleInterrupted)
}

func (o *Observer) observeSubagent(ctx context.Context, event coding.TelemetryEvent) {
	attrs := []attribute.KeyValue{
		attribute.String("coding.subagent.role", event.SubagentRole),
		attribute.String("coding.subagent.state", event.SubagentState),
		attribute.String("coding.subagent.model", event.ModelID),
		attribute.String("coding.subagent.code", event.Code),
		attribute.String("coding.subagent.stop", string(event.Stop)),
		attribute.Int("coding.subagent.turns", event.Turns),
		attribute.Int("coding.subagent.tool_calls", event.ToolCalls),
		attribute.Int64("coding.subagent.duration_ms", event.DurationMillis),
		attribute.Int("coding.usage.input_tokens", event.Usage.InputTokens),
		attribute.Int("coding.usage.output_tokens", event.Usage.OutputTokens),
		attribute.Int("coding.usage.reasoning_tokens", event.Usage.ReasoningTokens),
	}
	metricAttrs := []attribute.KeyValue{
		attribute.String("coding.subagent.role", event.SubagentRole),
		attribute.String("coding.subagent.state", event.SubagentState),
	}
	o.subagents.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
	if event.Type == coding.EventSubagentCompleted || event.Type == coding.EventSubagentFailed ||
		event.Type == coding.EventSubagentCanceled || event.Type == coding.EventSubagentInterrupted {
		o.subagentDuration.Record(
			ctx,
			event.DurationMillis,
			metric.WithAttributes(metricAttrs...),
		)
	}
	o.instant(ctx, "coding."+string(event.Type), event, attrs, event.Failed)
}

func compactionLifecycle(eventType coding.EventType) string {
	if eventType == coding.EventCompactionStarted {
		return "started"
	}

	return "completed"
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

func (o *Observer) observeQuestion(ctx context.Context, event coding.TelemetryEvent) {
	state := "required"
	attrs := []attribute.KeyValue{attribute.Int("coding.question.count", event.Questions)}
	switch event.Type {
	case coding.EventQuestionResolved:
		state = "resolved"
		attrs = []attribute.KeyValue{
			attribute.Int("coding.question.answers", event.Answers),
			attribute.Bool("coding.question.chat", event.Chat),
		}
	case coding.EventQuestionRejected:
		state = "rejected"
	case coding.EventQuestionRequired:
	default:
		return
	}
	attrs = append(attrs, attribute.String("coding.question.state", state))
	o.questions.Add(ctx, 1, metric.WithAttributes(attrs...))
	o.instant(ctx, "coding.question."+state, event, attrs, false)
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
