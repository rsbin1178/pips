//nolint:wsl_v5 // Signal setup and assertions intentionally remain in lifecycle groups.
package otel

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestObserverEmitsContentFreeProductSignals(t *testing.T) {
	t.Parallel()

	tracerProvider := newTracerProviderSpy()
	meterProvider := newMeterProviderSpy()

	observer, err := New(Config{
		TracerProvider: tracerProvider,
		MeterProvider:  meterProvider,
	})
	require.NoError(t, err)

	at := time.Date(2026, time.July, 21, 10, 0, 0, 0, time.UTC)
	events := []coding.TelemetryEvent{
		{
			Type: coding.EventSessionOpened, Time: at,
			Provider: ai.ProviderOpenAI, ModelID: "test-model", Resumed: true,
		},
		{
			Type: coding.EventApprovalRequired, Time: at.Add(time.Second), Tool: "shell",
		},
		{
			Type: coding.EventQuestionRequired, Time: at.Add(1500 * time.Millisecond), Questions: 2,
		},
		{
			Type: coding.EventQuestionResolved, Time: at.Add(1600 * time.Millisecond),
			Answers: 2,
		},
		{
			Type: coding.EventModeChanged, Time: at.Add(1700 * time.Millisecond), Mode: coding.ModePlan,
		},
		{
			Type: coding.EventWorkspaceChanged, Time: at.Add(2 * time.Second), Changes: 2,
		},
		{
			Type: coding.EventSessionTreeChanged, Time: at.Add(2500 * time.Millisecond), Nodes: 4,
		},
		{
			Type: coding.EventSessionNavigated, Time: at.Add(2600 * time.Millisecond), Code: "navigated",
		},
		{
			Type: coding.EventSessionForked, Time: at.Add(2700 * time.Millisecond), Code: "forked",
		},
		{
			Type: coding.EventCompactionStarted, Time: at.Add(2800 * time.Millisecond),
			CompactionMode: coding.CompactionAutomatic, TokensBefore: 3000,
		},
		{
			Type: coding.EventCompactionCompleted, Time: at.Add(2900 * time.Millisecond),
			CompactionMode: coding.CompactionAutomatic, TokensBefore: 3000,
			TokensAfter: 900, DurationMillis: 25,
		},
		{
			Type: coding.EventIntegrationDiagnostic, Time: at.Add(3 * time.Second),
			Component: "mcp", Code: "connect_failed", Failed: true,
		},
		{
			Type: coding.EventInteractionCompleted, Time: at.Add(4 * time.Second),
			Outcome: coding.InteractionFailed, DurationMillis: 250, Failed: true,
			Usage: coding.TokenUsage{InputTokens: 11, OutputTokens: 3},
		},
		{
			Type: coding.EventSubagentCompleted, Time: at.Add(4500 * time.Millisecond),
			SubagentRole: "explore", SubagentState: "succeeded",
			ModelID: "openai/test", Code: "ok", Turns: 2, ToolCalls: 8,
			DurationMillis: 125, Usage: coding.TokenUsage{InputTokens: 100, OutputTokens: 20},
		},
		{
			Type: coding.EventTeamLifecycle, Time: at.Add(4750 * time.Millisecond),
			TeamScope: "attempt",
			TeamState: string(coding.TeamLifecycleCompleted), TeamActivity: "capturing",
			Code: "completed", Turns: 3, ToolCalls: 9, DurationMillis: 250,
			Usage: coding.TokenUsage{InputTokens: 120, OutputTokens: 30},
		},
		{
			Type: coding.EventTeamIntegrationLifecycle, Time: at.Add(4800 * time.Millisecond),
			IntegrationState:        string(coding.TeamIntegrationVerified),
			IntegrationVerification: "passed", Attempts: 2, Files: 4,
		},
		{
			Type: coding.EventSessionClosed, Time: at.Add(5 * time.Second), Code: "closed",
		},
	}
	for _, event := range events {
		require.NoError(t, observer.Observe(t.Context(), event))
	}

	spans := tracerProvider.snapshot()
	require.Len(t, spans, len(events))
	names := make([]string, len(spans))
	for index, span := range spans {
		names[index] = span.name
		assert.Equal(t, instrumentationName, span.scope)
		assert.NotContains(t, fmt.Sprint(span.attributes), "prompt-secret")
	}
	assert.ElementsMatch(t, []string{
		"coding.session.opened",
		"coding.approval.required",
		"coding.question.required",
		"coding.question.resolved",
		"coding.mode.changed",
		"coding.workspace.changed",
		"coding.session.tree.changed",
		"coding.session.navigated",
		"coding.session.forked",
		"coding.compaction.started",
		"coding.compaction.completed",
		"coding.integration.diagnostic",
		"coding.interaction",
		"coding.subagent.completed",
		"coding.team.lifecycle",
		"coding.team.integration",
		"coding.session.closed",
	}, names)

	interactionIndex := slices.IndexFunc(spans, func(span spanRecord) bool {
		return span.name == "coding.interaction"
	})
	require.NotEqual(t, -1, interactionIndex)
	assert.Equal(t, 250*time.Millisecond, spans[interactionIndex].ended.Sub(spans[interactionIndex].started))
	assert.Equal(t, codes.Error, spans[interactionIndex].status)
	assert.Equal(t, instrumentationName, meterProvider.scope)
	assert.ElementsMatch(t, []string{
		"pips.coding.sessions",
		"pips.coding.interactions",
		"pips.coding.interaction.duration",
		"pips.coding.approvals",
		"pips.coding.questions",
		"pips.coding.workspace.changes",
		"pips.coding.integration.diagnostics",
		"pips.coding.subagents",
		"pips.coding.subagent.duration",
		"pips.coding.teams",
		"pips.coding.team.duration",
		"pips.coding.team.integrations",
	}, meterProvider.instrumentNames())
}

func TestObserverAcceptsGlobalProviders(t *testing.T) {
	t.Parallel()

	observer, err := New(Config{})
	require.NoError(t, err)
	require.NoError(t, observer.Observe(t.Context(), coding.TelemetryEvent{}))
}

type spanRecord struct {
	name       string
	scope      string
	started    time.Time
	ended      time.Time
	attributes []attribute.KeyValue
	status     codes.Code
}

type tracerProviderSpy struct {
	trace.TracerProvider
	mu    sync.Mutex
	spans []*spanRecord
}

func newTracerProviderSpy() *tracerProviderSpy {
	return &tracerProviderSpy{TracerProvider: tracenoop.NewTracerProvider()}
}

func (p *tracerProviderSpy) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	return &tracerSpy{
		Tracer: p.TracerProvider.Tracer(name, options...),
		owner:  p,
		scope:  name,
	}
}

func (p *tracerProviderSpy) snapshot() []spanRecord {
	p.mu.Lock()
	defer p.mu.Unlock()

	spans := make([]spanRecord, len(p.spans))
	for index, span := range p.spans {
		spans[index] = *span
		spans[index].attributes = slices.Clone(span.attributes)
	}

	return spans
}

type tracerSpy struct {
	trace.Tracer
	owner *tracerProviderSpy
	scope string
}

func (t *tracerSpy) Start(
	ctx context.Context,
	name string,
	options ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	config := trace.NewSpanStartConfig(options...)
	record := &spanRecord{
		name:       name,
		scope:      t.scope,
		started:    config.Timestamp(),
		attributes: slices.Clone(config.Attributes()),
	}

	t.owner.mu.Lock()
	t.owner.spans = append(t.owner.spans, record)
	t.owner.mu.Unlock()

	spanCtx, span := t.Tracer.Start(ctx, name, options...)

	return spanCtx, &spanSpy{Span: span, record: record, owner: t.owner}
}

type spanSpy struct {
	trace.Span
	record *spanRecord
	owner  *tracerProviderSpy
}

func (s *spanSpy) SetStatus(code codes.Code, description string) {
	s.owner.mu.Lock()
	s.record.status = code
	s.owner.mu.Unlock()
	s.Span.SetStatus(code, description)
}

func (s *spanSpy) End(options ...trace.SpanEndOption) {
	config := trace.NewSpanEndConfig(options...)

	s.owner.mu.Lock()
	s.record.ended = config.Timestamp()
	s.owner.mu.Unlock()
	s.Span.End(options...)
}

type meterProviderSpy struct {
	metric.MeterProvider
	mu          sync.Mutex
	scope       string
	instruments []string
}

func newMeterProviderSpy() *meterProviderSpy {
	return &meterProviderSpy{MeterProvider: metricnoop.NewMeterProvider()}
}

func (p *meterProviderSpy) Meter(name string, options ...metric.MeterOption) metric.Meter {
	p.mu.Lock()
	p.scope = name
	p.mu.Unlock()

	return &meterSpy{
		Meter: p.MeterProvider.Meter(name, options...),
		owner: p,
	}
}

func (p *meterProviderSpy) instrumentNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return slices.Clone(p.instruments)
}

type meterSpy struct {
	metric.Meter
	owner *meterProviderSpy
}

func (m *meterSpy) Int64Counter(
	name string,
	options ...metric.Int64CounterOption,
) (metric.Int64Counter, error) {
	m.owner.mu.Lock()
	m.owner.instruments = append(m.owner.instruments, name)
	m.owner.mu.Unlock()

	return m.Meter.Int64Counter(name, options...)
}

func (m *meterSpy) Int64Histogram(
	name string,
	options ...metric.Int64HistogramOption,
) (metric.Int64Histogram, error) {
	m.owner.mu.Lock()
	m.owner.instruments = append(m.owner.instruments, name)
	m.owner.mu.Unlock()

	return m.Meter.Int64Histogram(name, options...)
}
