package harness_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func addTool() agent.Tool {
	return agent.NewTool("add", "Add two integers.",
		func(_ context.Context, args struct {
			A int `json:"a"`
			B int `json:"b"`
		},
		) (string, error) {
			return strconv.Itoa(args.A + args.B), nil
		})
}

func TestHarnessPromptPersistsRun(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m",
		callResponse("c1", "add", `{"a":2,"b":3}`),
		textResponse("It is 5.", 30),
	)

	sess := buildSession(t)

	h, err := harness.New(model, sess, harness.WithTools(addTool()), harness.WithSystem("Be terse."))
	require.NoError(t, err)

	result, err := h.Prompt(t.Context(), "2+3?")
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, "It is 5.", result.Text())
	assert.Equal(t, harness.PhaseIdle, h.Phase())

	// The tree holds the full exchange: user, assistant(call), tool, final.
	entries := sess.Entries()
	require.Len(t, entries, 4)

	for _, e := range entries {
		assert.Equal(t, harness.KindMessage, e.Kind)
	}

	// Assistant entries carry their turn's usage.
	require.NotNil(t, entries[1].Usage)
	assert.Equal(t, 10, entries[1].Usage.InputTokens)
	require.NotNil(t, entries[3].Usage)
	assert.Equal(t, 30, entries[3].Usage.InputTokens)
	assert.Nil(t, entries[0].Usage)
	assert.Nil(t, entries[2].Usage)

	// The system prompt reached the model.
	assert.Equal(t, "Be terse.", model.Requests()[0].System)
}

func TestHarnessResumesAcrossInstances(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/s.jsonl"

	store, err := harness.CreateJSONL(path, "", nil)
	require.NoError(t, err)

	sess, err := harness.NewSession(store)
	require.NoError(t, err)

	first := newScriptedModel("m", textResponse("first answer", 10))

	h, err := harness.New(first, sess)
	require.NoError(t, err)
	_, err = h.Prompt(t.Context(), "first question")
	require.NoError(t, err)
	require.NoError(t, store.Close())

	// A new process: reopen the file and continue the conversation.
	reopened, err := harness.OpenJSONL(path)
	require.NoError(t, err)

	defer reopened.Close() //nolint:errcheck // test cleanup

	restored, err := harness.NewSession(reopened)
	require.NoError(t, err)

	second := newScriptedModel("m", textResponse("second answer", 20))

	h2, err := harness.New(second, restored)
	require.NoError(t, err)

	result, err := h2.Prompt(t.Context(), "second question")
	require.NoError(t, err)
	assert.Equal(t, "second answer", result.Text())

	// The restored history reached the model: first Q, first A, second Q.
	reqs := second.Requests()
	require.Len(t, reqs, 1)
	assert.Len(t, reqs[0].Messages, 3)
}

func TestHarnessAutoCompaction(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	// Seed history whose recorded usage exceeds the threshold.
	appendText(t, sess, ai.RoleUser, bigText(100), nil)
	appendText(t, sess, ai.RoleAssistant, "big turn", &ai.Usage{InputTokens: 90_000, OutputTokens: 500})
	appendText(t, sess, ai.RoleUser, bigText(100), nil)
	appendText(t, sess, ai.RoleAssistant, "done", nil)

	summarizer := newScriptedModel("sum", textResponse("## Goal\nSummarized.", 10))
	model := newScriptedModel("m", textResponse("fresh answer", 100))

	h, err := harness.New(model, sess,
		harness.WithCompaction(harness.CompactionSettings{ContextTokens: 100_000, KeepRecentTokens: 50}),
		harness.WithSummaryModel(summarizer),
	)
	require.NoError(t, err)

	result, err := h.Prompt(t.Context(), "next task")
	require.NoError(t, err)
	assert.Equal(t, "fresh answer", result.Text())

	// The summarizer ran before the main model, and the main model saw the
	// compacted view (summary message instead of the old history).
	require.Len(t, summarizer.Requests(), 1)
	reqs := model.Requests()
	require.Len(t, reqs, 1)

	lead, ok := reqs[0].Messages[0].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, lead.Text, harness.CompactionPrefix)

	// The compaction entry was committed to the tree.
	kinds := make(map[harness.Kind]int)
	for _, e := range sess.Entries() {
		kinds[e.Kind]++
	}

	assert.Equal(t, 1, kinds[harness.KindCompaction])
}

func TestHarnessBusy(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})

	blocking := agent.NewTool("block", "Blocks.",
		func(_ context.Context, _ struct{}) (string, error) {
			close(started)
			<-release

			return "ok", nil
		})

	model := newScriptedModel("m",
		callResponse("c1", "block", `{}`),
		textResponse("done", 10),
	)

	sess := buildSession(t)

	h, err := harness.New(model, sess, harness.WithTools(blocking))
	require.NoError(t, err)

	done := make(chan error, 1)

	go func() {
		_, promptErr := h.Prompt(context.Background(), "go")
		done <- promptErr
	}()

	<-started
	assert.Equal(t, harness.PhaseTurn, h.Phase())

	_, err = h.Prompt(t.Context(), "again")
	require.ErrorIs(t, err, harness.ErrBusy)
	require.ErrorIs(t, h.Compact(t.Context(), ""), harness.ErrBusy)
	require.ErrorIs(t, h.SetModel(model), harness.ErrBusy)

	// Steering works while running; idle steering is refused after.
	require.NoError(t, h.Steer(ai.UserText("nudge")))

	close(release)
	require.NoError(t, <-done)
	require.ErrorIs(t, h.Steer(ai.UserText("late")), harness.ErrIdle)
}

func TestHarnessSetModelRecordsChange(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	h, err := harness.New(newScriptedModel("first"), sess)
	require.NoError(t, err)

	replacement := newScriptedModel("second", textResponse("from second", 10))
	require.NoError(t, h.SetModel(replacement))

	result, err := h.Prompt(t.Context(), "hi")
	require.NoError(t, err)
	assert.Equal(t, "from second", result.Text())

	cctx, err := sess.Context()
	require.NoError(t, err)
	assert.Equal(t, "second", cctx.ModelID)
}

func TestHarnessNavigateWithSummary(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m", textResponse("branch a result", 10))
	summarizer := newScriptedModel("sum", textResponse("## What was attempted\nBranch a.", 10))
	sess := buildSession(t)

	h, err := harness.New(model, sess, harness.WithSummaryModel(summarizer))
	require.NoError(t, err)

	_, err = h.Prompt(t.Context(), "try branch a")
	require.NoError(t, err)

	root := sess.Entries()[0].ID

	require.NoError(t, h.NavigateTo(t.Context(), root, true))

	// The branch summary landed under the new position and enters context.
	cctx, err := sess.Context()
	require.NoError(t, err)
	require.Len(t, cctx.Messages, 2)

	summary, ok := cctx.Messages[1].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, summary.Text, "Branch a")
}

func TestHarnessPauseAndResolve(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m",
		callResponse("c1", "add", `{"a":1,"b":1}`),
		textResponse("resumed", 10),
	)

	sess := buildSession(t)

	h, err := harness.New(model, sess,
		harness.WithTools(addTool()),
		harness.WithAgentOptions(agent.WithBeforeTool(
			func(_ context.Context, _ agent.ToolCallInfo) agent.ToolDecision {
				return agent.ToolDecision{Action: agent.ToolDecisionPause}
			})),
	)
	require.NoError(t, err)

	result, err := h.Prompt(t.Context(), "go")
	require.NoError(t, err)
	require.Equal(t, agent.StopPaused, result.Stop)

	// Resolve out-of-band; the resolution persists to the tree.
	err = h.ResolvePending(t.Context(), func(_ context.Context, _ ai.ToolCallPart) ([]ai.Part, error) {
		return agent.TextResult("2"), nil
	})
	require.NoError(t, err)

	// A follow-up prompt continues from a protocol-complete tree. The gate
	// still pauses tools, but this turn has none.
	final, err := h.Prompt(t.Context(), "and then?")
	require.NoError(t, err)
	assert.Equal(t, "resumed", final.Text())
}

func TestHarnessResolvesDurablePendingSubset(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m",
		&ai.Response{
			Message: ai.Assistant(
				ai.ToolCallPart{ID: "c1", Name: "first"},
				ai.ToolCallPart{ID: "c2", Name: "second"},
			),
			FinishReason: ai.FinishToolCalls,
		},
		textResponse("resumed after both decisions", 10),
	)
	sess := buildSession(t)

	h, err := harness.New(model, sess, harness.WithAgentOptions(agent.WithBeforeTool(
		func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
			return agent.ToolDecision{Action: agent.ToolDecisionPause}
		},
	)))
	require.NoError(t, err)

	result, err := h.Prompt(t.Context(), "go")
	require.NoError(t, err)
	require.Equal(t, agent.StopPaused, result.Stop)
	require.Len(t, result.Pending, 2)

	require.NoError(t, h.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: "c2", Content: agent.TextResult("approved second"),
	}))

	cctx, err := sess.Context()
	require.NoError(t, err)

	pending := agent.NewSession(cctx.Messages...).Pending()
	require.Len(t, pending, 1)
	assert.Equal(t, "c1", pending[0].ID)

	// The unresolved call remains durable, so another prompt cannot bypass it.
	entriesBefore := len(sess.Entries())
	_, err = h.Prompt(t.Context(), "too early")
	require.ErrorIs(t, err, agent.ErrPendingToolCalls)
	assert.Len(t, sess.Entries(), entriesBefore, "rejected prompt is not persisted")

	require.NoError(t, h.ResolveToolCalls(agent.ToolResolution{
		ToolCallID: "c1", Content: agent.TextResult("rejected first"), IsError: true,
	}))

	final, err := h.Prompt(t.Context(), "continue")
	require.NoError(t, err)
	assert.Equal(t, "resumed after both decisions", final.Text())
}

func TestHarnessPendingApprovalPrecedesAutomaticCompaction(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)
	appendText(t, sess, ai.RoleUser, bigText(100), nil)

	_, err := sess.AppendMessage(ai.Assistant(
		ai.ToolCallPart{ID: "c1", Name: "approval"},
	), &ai.Usage{InputTokens: 90_000})
	require.NoError(t, err)

	summarizer := newScriptedModel("summary", textResponse("must not compact", 10))
	h, err := harness.New(newScriptedModel("main"), sess,
		harness.WithCompaction(harness.CompactionSettings{ContextTokens: 100_000}),
		harness.WithSummaryModel(summarizer),
	)
	require.NoError(t, err)

	entriesBefore := len(sess.Entries())
	_, err = h.Prompt(t.Context(), "must wait")
	require.ErrorIs(t, err, agent.ErrPendingToolCalls)
	assert.Len(t, sess.Entries(), entriesBefore)
	assert.Empty(t, summarizer.Requests())
}

func TestHarnessPromptStreamMatchesObserverAndPersists(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m",
		callResponse("c1", "add", `{"a":2,"b":3}`),
		textResponse("It is 5.", 30),
	)
	sess := buildSession(t)

	var observed []agent.EventType

	h, err := harness.New(model, sess,
		harness.WithTools(addTool()),
		harness.WithOnEvent(func(_ context.Context, ev agent.Event) {
			observed = append(observed, ev.Type())
		}),
	)
	require.NoError(t, err)

	var streamed []agent.EventType

	for ev, err := range h.PromptStream(t.Context(), "2+3?") {
		require.NoError(t, err)

		streamed = append(streamed, ev.Type())
	}

	assert.Equal(t, observed, streamed)
	assert.Equal(t, harness.PhaseIdle, h.Phase())
	require.ErrorIs(t, h.Cancel(), harness.ErrIdle)

	entries := sess.Entries()
	require.Len(t, entries, 4)

	text, ok := entries[3].Message.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "It is 5.", text.Text)
}

func TestHarnessInputGuardrailRejectsBeforePersistentPrompt(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m", textResponse("unused", 10))
	sess := buildSession(t)
	h, err := harness.New(model, sess, harness.WithAgentOptions(
		agent.WithInputGuardrail(
			"policy",
			func(context.Context, agent.InputGuardrailInfo) error {
				return errors.New("rejected")
			},
		),
	))
	require.NoError(t, err)

	result, err := h.Prompt(t.Context(), "do not retain")
	require.ErrorIs(t, err, agent.ErrGuardrail)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.RunID)
	assert.Empty(t, sess.Entries())
	assert.Empty(t, model.Requests())
	assert.Equal(t, harness.PhaseIdle, h.Phase())
}

func TestHarnessPromptStopsWhenPromptPersistenceFails(t *testing.T) {
	t.Parallel()

	sess, err := harness.NewSession(rejectingAppendStore{})
	require.NoError(t, err)

	model := newScriptedModel("m", textResponse("must not run", 10))
	h, err := harness.New(model, sess)
	require.NoError(t, err)

	_, err = h.Prompt(t.Context(), "cannot persist")
	require.ErrorIs(t, err, errRejectedAppend)
	assert.Empty(t, model.Requests())
	assert.Empty(t, sess.Entries())
	assert.Equal(t, harness.PhaseIdle, h.Phase())
}

func TestHarnessPromptStreamEarlyBreakCleansUp(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m", textResponse("provisional", 10))
	sess := buildSession(t)
	h, err := harness.New(model, sess)
	require.NoError(t, err)

	for ev, streamErr := range h.PromptStream(t.Context(), "go") {
		require.NoError(t, streamErr)

		if event, ok := ev.Payload().(agent.ModelStreamEvent); ok &&
			event.Event.Type == ai.StreamTextDelta {
			break
		}
	}

	assert.Equal(t, harness.PhaseIdle, h.Phase())
	require.ErrorIs(t, h.Cancel(), harness.ErrIdle)

	entries := sess.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, ai.RoleUser, entries[0].Message.Role)

	text, ok := entries[0].Message.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "go", text.Text)
}

func TestHarnessCancelActivePrompt(t *testing.T) {
	t.Parallel()

	model := newBlockingModel()
	sess := buildSession(t)
	h, err := harness.New(model, sess)
	require.NoError(t, err)

	done := make(chan error, 1)

	go func() {
		_, promptErr := h.Prompt(context.Background(), "wait")
		done <- promptErr
	}()

	<-model.started
	require.NoError(t, h.Cancel())
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, harness.PhaseIdle, h.Phase())
	require.ErrorIs(t, h.Cancel(), harness.ErrIdle)
	require.Len(t, sess.Entries(), 1)
}

func TestHarnessPromptStreamCallerCancellation(t *testing.T) {
	t.Parallel()

	model := newBlockingModel()
	sess := buildSession(t)
	h, err := harness.New(model, sess)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		var streamErr error

		for _, err := range h.PromptStream(ctx, "wait") {
			if err != nil {
				streamErr = err
			}
		}

		done <- streamErr
	}()

	<-model.started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, harness.PhaseIdle, h.Phase())
}

func TestHarnessOnEventForwarding(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m", textResponse("hi", 10))
	sess := buildSession(t)

	var events []agent.EventType

	h, err := harness.New(model, sess, harness.WithOnEvent(func(_ context.Context, ev agent.Event) {
		events = append(events, ev.Type())
	}))
	require.NoError(t, err)

	_, err = h.Prompt(t.Context(), "hello")
	require.NoError(t, err)

	assert.Contains(t, events, agent.EventRunStarted)
	assert.Contains(t, events, agent.EventRunCompleted)
}

func TestHarnessPromptTemplate(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m", textResponse("done", 10))
	sess := buildSession(t)

	h, err := harness.New(model, sess, harness.WithTemplates(
		harness.PromptTemplate{Name: "review", Content: "Review $1 focusing on $2."},
	))
	require.NoError(t, err)

	_, err = h.PromptTemplate(t.Context(), "review", "main.go", "errors")
	require.NoError(t, err)

	prompt, ok := model.Requests()[0].Messages[0].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "Review main.go focusing on errors.", prompt.Text)

	_, err = h.PromptTemplate(t.Context(), "missing")
	require.ErrorContains(t, err, "unknown prompt template")
}

func TestHarnessSteeringMidRun(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("m",
		callResponse("c1", "poke", `{}`),
		textResponse("steered", 10),
	)

	sess := buildSession(t)

	var h *harness.Harness

	poke := agent.NewTool("poke", "Pokes.",
		func(_ context.Context, _ struct{}) (string, error) {
			require.NoError(t, h.Steer(ai.UserText("change of plans")))
			return "ok", nil
		})

	h, err := harness.New(model, sess, harness.WithTools(poke))
	require.NoError(t, err)

	_, err = h.Prompt(t.Context(), "go")
	require.NoError(t, err)

	// The steered message reached the model and was persisted to the tree.
	require.Len(t, model.Requests(), 2)

	found := false

	for _, e := range sess.Entries() {
		if e.Kind == harness.KindMessage && e.Message.Role == ai.RoleUser {
			if text, ok := e.Message.Parts[0].(ai.TextPart); ok && text.Text == "change of plans" {
				found = true
			}
		}
	}

	assert.True(t, found, "steered message persisted")
}

func TestHarnessSaveOnRunError(t *testing.T) {
	t.Parallel()

	// Turn one succeeds with a tool call; turn two's model call fails by
	// exhausting the script returns text instead, so use a
	// cancelled context after the first turn via a tool.
	ctx, cancel := context.WithCancel(t.Context())

	stopper := agent.NewTool("stop", "Cancels the run.",
		func(_ context.Context, _ struct{}) (string, error) {
			cancel()
			return "stopping", nil
		})

	model := newScriptedModel("m", callResponse("c1", "stop", `{}`))
	sess := buildSession(t)

	h, err := harness.New(model, sess, harness.WithTools(stopper))
	require.NoError(t, err)

	_, err = h.PromptMessages(ctx, ai.UserText("go"))
	require.ErrorIs(t, err, context.Canceled)

	// Everything that completed was persisted: user, assistant, tool result.
	entries := sess.Entries()
	require.Len(t, entries, 3)
	assert.Equal(t, harness.PhaseIdle, h.Phase())
}

type blockingModel struct {
	started chan struct{}
	once    sync.Once
}

func newBlockingModel() *blockingModel {
	return &blockingModel{started: make(chan struct{})}
}

func (m *blockingModel) Generate(ctx context.Context, _ ai.Request) (*ai.Response, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()

	return nil, ctx.Err()
}

func (m *blockingModel) Stream(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		_, err := m.Generate(ctx, req)
		yield(ai.StreamEvent{}, err)
	}
}

func (*blockingModel) Provider() ai.Provider { return ai.Provider("blocking") }
func (*blockingModel) ModelID() string       { return "blocking-1" }
func (*blockingModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true}
}

var errRejectedAppend = errors.New("store rejected append")

type rejectingAppendStore struct{}

func (rejectingAppendStore) Metadata() harness.SessionMetadata {
	return harness.SessionMetadata{ID: "rejecting"}
}

func (rejectingAppendStore) Append(harness.Entry) error {
	return errRejectedAppend
}

func (rejectingAppendStore) Entries() ([]harness.Entry, error) {
	return nil, nil
}
