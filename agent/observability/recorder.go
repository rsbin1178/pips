// Package observability derives vendor-neutral traces and aggregate metrics
// from agent events. It owns no goroutines and does not export prompts or tool
// payloads; applications decide where and how to persist or export snapshots.
package observability

import (
	"context"
	"sync"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
)

// ToolSpan records one observed tool execution without retaining arguments or
// result content.
type ToolSpan struct {
	Name      string
	StartedAt time.Time
	EndedAt   time.Time
	Failed    bool
}

// RunTrace is an immutable observability view of one run.
type RunTrace struct {
	RunID       string
	ParentRunID string
	Agent       string
	StartedAt   time.Time
	EndedAt     time.Time
	Stop        agent.StopReason
	Turns       int
	Usage       ai.Usage
	Tools       []ToolSpan
}

// Metrics aggregates completed and in-flight observations. Counts are useful
// for a metrics exporter; usage is the sum reported by turn/run events.
type Metrics struct {
	RunsStarted   uint64
	RunsCompleted uint64
	Turns         uint64
	ToolCalls     uint64
	ToolFailures  uint64
	Usage         ai.Usage
}

// Recorder consumes events through its Observe method, suitable for passing to
// agent.WithOnEvent. It is safe for concurrent agent runs.
type Recorder struct {
	mu      sync.RWMutex
	runs    map[string]*RunTrace
	active  map[string]map[string]int
	metrics Metrics
}

// NewRecorder creates an empty recorder.
func NewRecorder() *Recorder {
	return &Recorder{runs: make(map[string]*RunTrace), active: make(map[string]map[string]int)}
}

// Observe records one normalized agent event. It deliberately retains only
// lifecycle metadata, names, timestamps, stop reason, and usage.
//
//nolint:gocyclo // Event variants intentionally map to distinct aggregate fields.
func (r *Recorder) Observe(_ context.Context, event agent.Event) {
	if r == nil || event.RunID == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	trace := r.ensure(event)
	switch payload := event.Payload().(type) {
	case agent.RunStarted:
		r.metrics.RunsStarted++
	case agent.TurnStarted, agent.ModelStreamEvent, agent.MessageCommitted,
		agent.CandidateDiscarded, agent.ToolUpdated:
		// These events carry no durable aggregate currently.
	case agent.TurnCompleted:
		trace.Turns = max(trace.Turns, payload.Turn)
		trace.Usage = payload.Usage
		r.metrics.Turns++
	case agent.ToolStarted:
		r.metrics.ToolCalls++

		trace.Tools = append(trace.Tools, ToolSpan{Name: payload.Call.Name, StartedAt: event.Time})
		r.active[event.RunID][payload.Call.ID] = len(trace.Tools) - 1
	case agent.ToolCompleted:
		if index, ok := r.active[event.RunID][payload.Call.ID]; ok {
			trace.Tools[index].EndedAt = event.Time

			trace.Tools[index].Failed = payload.Result.IsError
			if trace.Tools[index].Failed {
				r.metrics.ToolFailures++
			}

			delete(r.active[event.RunID], payload.Call.ID)
		}
	case agent.RunCompleted:
		trace.EndedAt, trace.Stop, trace.Usage = event.Time, payload.Stop, payload.Usage
		trace.Turns = max(trace.Turns, payload.Turns)
		r.metrics.RunsCompleted++
		r.metrics.Usage.Add(payload.Usage)
	}
}

// Trace returns a copy of a run trace and false when it is unknown.
func (r *Recorder) Trace(runID string) (RunTrace, bool) {
	if r == nil {
		return RunTrace{}, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	trace, ok := r.runs[runID]
	if !ok {
		return RunTrace{}, false
	}

	return cloneTrace(*trace), true
}

// Metrics returns an aggregate snapshot.
func (r *Recorder) Metrics() Metrics {
	if r == nil {
		return Metrics{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.metrics
}

func (r *Recorder) ensure(event agent.Event) *RunTrace {
	if trace, ok := r.runs[event.RunID]; ok {
		return trace
	}

	trace := &RunTrace{RunID: event.RunID, ParentRunID: event.ParentRunID, Agent: event.Agent, StartedAt: event.Time}
	r.runs[event.RunID] = trace
	r.active[event.RunID] = make(map[string]int)

	return trace
}

func cloneTrace(trace RunTrace) RunTrace {
	trace.Tools = append([]ToolSpan(nil), trace.Tools...)

	return trace
}
