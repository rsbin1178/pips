//nolint:wsl_v5 // Scenarios keep their arrangement, mutation and assertion adjacent.
package tui

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/require"
)

// viewportProjectionBlocks is the cached frame's block list.
func viewportProjectionBlocks(model *Model) []timelineBlock {
	return viewportProjectionFrameBlocks(model.viewportProjection())
}

// viewportProjectionFrameBlocks flattens one cached frame, prefix then tail.
func viewportProjectionFrameBlocks(frame frameProjection) []timelineBlock {
	blocks := make([]timelineBlock, 0, len(frame.prefixEntries)+len(frame.tailEntries))
	for _, entry := range frame.prefixEntries {
		blocks = append(blocks, entry.block)
	}
	for _, entry := range frame.tailEntries {
		blocks = append(blocks, entry.block)
	}

	return blocks
}

// projectionCacheModel builds a fullscreen model over a small conversation that
// carries visible reasoning, so the thinking-visibility toggle has something to
// change.
func projectionCacheModel(t *testing.T) *Model {
	t.Helper()

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("hello"),
		ai.Assistant(
			ai.Text("hi"),
			ai.ReasoningPart{Text: "reasoned"},
		),
	}
	state.MessageCandidates = []coding.CandidateIdentity{
		{RunID: "run-1", Turn: 1},
		{RunID: "run-1", Turn: 2},
	}

	model := fullscreenModel(t, stubController{state: state}, false)
	model.state = state
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	return model
}

// projectionRunningReadTool is a live tool the volatile tail carries until it
// settles.
func projectionRunningReadTool() coding.ToolState {
	return coding.ToolState{
		RunID: "run-1", Turn: 3,
		Call: coding.ToolCall{
			ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"main.go"}`),
		},
		Status: coding.ToolStatusRunning,
	}
}

// projectionRunningSubagentTool is the live delegation the subagent lifecycle
// enriches.
func projectionRunningSubagentTool() coding.ToolState {
	return coding.ToolState{
		RunID: "run-1", Turn: 3,
		Call: coding.ToolCall{
			ID: "call-sub", Name: subagent.ToolName,
			Arguments: ai.JSON(`{"role":"explore","task":"Inspect the timeline"}`),
		},
		Status: coding.ToolStatusRunning,
	}
}

// projectionCacheScenario mutates a warmed model. arrange, when set, runs before
// the cache is warmed so the scenario can start from the state it mutates.
type projectionCacheScenario struct {
	name    string
	arrange func(t *testing.T, model *Model)
	mutate  func(t *testing.T, model *Model)
}

// TestProjectionCacheMatchesPureProjection pins requirement D4: the incremental
// frame is exactly the pure projection, before and after every input surface
// changes. Each scenario warms the cache first, mutates, then compares, so a
// stale prefix cannot hide behind a cold cache.
func TestProjectionCacheMatchesPureProjection(t *testing.T) {
	t.Parallel()

	scenarios := []projectionCacheScenario{
		{
			name: "append committed message",
			mutate: func(_ *testing.T, model *Model) {
				model.state.Transcript = append(model.state.Transcript, ai.UserText("follow up"))
				model.state.MessageCandidates = append(
					model.state.MessageCandidates,
					coding.CandidateIdentity{RunID: "run-1", Turn: 3},
				)
			},
		},
		{
			name: "replace transcript",
			mutate: func(_ *testing.T, model *Model) {
				model.state.Transcript = []ai.Message{
					ai.UserText("compacted"),
					ai.AssistantText("summary"),
					ai.UserText("more"),
				}
				model.state.MessageCandidates = nil
			},
		},
		{
			name: "tool started",
			mutate: func(_ *testing.T, model *Model) {
				model.state.Tools = append(model.state.Tools, projectionRunningReadTool())
			},
		},
		{
			name: "tool progress updated",
			arrange: func(_ *testing.T, model *Model) {
				model.state.Tools = []coding.ToolState{projectionRunningReadTool()}
			},
			mutate: func(_ *testing.T, model *Model) {
				model.state.Tools[0].Update = []ai.Part{ai.Text("progress 2")}
			},
		},
		{
			name: "tool completed",
			arrange: func(_ *testing.T, model *Model) {
				model.state.Tools = []coding.ToolState{projectionRunningReadTool()}
			},
			mutate: func(_ *testing.T, model *Model) {
				model.state.Tools[0].Status = coding.ToolStatusCompleted
				model.state.Tools[0].Result = codingToolResultFor("call-1", "read", "file contents")
			},
		},
		{
			name: "subagent appended",
			arrange: func(_ *testing.T, model *Model) {
				model.state.Tools = []coding.ToolState{projectionRunningSubagentTool()}
			},
			mutate: func(_ *testing.T, model *Model) {
				model.state.Subagents = append(model.state.Subagents, coding.SubagentState{
					ChildSessionID: "child-1", ParentToolCallID: "call-sub",
					Role: subagent.RoleExplore, State: subagent.StateRunning,
					TaskPreview: "Inspect the timeline",
					Activity: subagent.ActivitySummary{
						Action: subagent.ActivityActionRead, Target: "main.go",
					},
					ToolCalls: 2, DurationMillis: 1500,
				})
			},
		},
		{
			name: "subagent updated",
			arrange: func(_ *testing.T, model *Model) {
				model.state.Tools = []coding.ToolState{projectionRunningSubagentTool()}
				model.state.Subagents = []coding.SubagentState{{
					ChildSessionID: "child-1", ParentToolCallID: "call-sub",
					Role: subagent.RoleExplore, State: subagent.StateRunning,
					TaskPreview: "Inspect the timeline",
				}}
			},
			mutate: func(_ *testing.T, model *Model) {
				model.state.Subagents[0].State = subagent.StateSucceeded
				model.state.Subagents[0].Activity = subagent.ActivitySummary{
					Action: subagent.ActivityActionSearch, Target: "timeline.go",
				}
				model.state.Subagents[0].ToolCalls = 5
				model.state.Subagents[0].DurationMillis = 4200
			},
		},
		{
			name: "team lifecycles appended",
			mutate: func(_ *testing.T, model *Model) {
				model.state.Teams = append(model.state.Teams, coding.TeamLifecycleState{
					TeamLifecycle: coding.TeamLifecycle{
						TeamID: "team-1", MemberID: "member-1", TaskID: "task-1",
						AttemptID: "attempt-1", State: coding.TeamLifecycleRunning,
						Activity: coding.TeamActivityWorking,
					},
				})
				model.state.TeamControls = append(model.state.TeamControls,
					coding.TeamControlLifecycleState{
						TeamControlLifecycle: coding.TeamControlLifecycle{
							TeamID: "team-1", Revision: 1, CommandID: "command-1",
							Action: coding.TeamControlMessage, State: coding.TeamControlPending,
						},
					})
				model.state.TeamIntegrations = append(model.state.TeamIntegrations,
					coding.TeamIntegrationLifecycleState{
						TeamIntegrationLifecycle: coding.TeamIntegrationLifecycle{
							TeamID: "team-1", IntegrationID: "integration-1",
							State: coding.TeamIntegrationReady,
						},
					})
			},
		},
		{
			name: "draft growth",
			mutate: func(_ *testing.T, model *Model) {
				model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "partial answer"})
				model.state.DraftCandidate = coding.CandidateIdentity{RunID: "run-1", Turn: 3}
			},
		},
		{
			name: "draft commit",
			arrange: func(_ *testing.T, model *Model) {
				model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "partial answer"})
				model.state.DraftCandidate = coding.CandidateIdentity{RunID: "run-1", Turn: 3}
			},
			mutate: func(_ *testing.T, model *Model) {
				model.state.Draft = coding.StreamDraft{}
				model.state.DraftCandidate = coding.CandidateIdentity{}
				model.state.Transcript = append(model.state.Transcript, ai.AssistantText("committed answer"))
				model.state.MessageCandidates = append(
					model.state.MessageCandidates,
					coding.CandidateIdentity{RunID: "run-1", Turn: 3},
				)
			},
		},
		{
			name: "commit several messages at once",
			mutate: func(_ *testing.T, model *Model) {
				model.state.Transcript = append(
					model.state.Transcript,
					ai.AssistantText("first"),
					ai.UserText("second"),
					ai.AssistantText("third"),
				)
				model.state.MessageCandidates = append(
					model.state.MessageCandidates,
					coding.CandidateIdentity{RunID: "run-1", Turn: 3},
					coding.CandidateIdentity{},
					coding.CandidateIdentity{RunID: "run-1", Turn: 4},
				)
			},
		},
		{
			name: "commit a tool result",
			arrange: func(_ *testing.T, model *Model) {
				model.state.Tools = []coding.ToolState{projectionRunningReadTool()}
			},
			mutate: func(_ *testing.T, model *Model) {
				model.state.Transcript = append(
					model.state.Transcript,
					codingToolResultFor("call-1", "read", "file contents"),
				)
				model.state.MessageCandidates = append(
					model.state.MessageCandidates, coding.CandidateIdentity{},
				)
			},
		},
		{
			name: "commit text message with a pending marker",
			arrange: func(_ *testing.T, model *Model) {
				model.completionMarkers = append(model.completionMarkers, completionMarker{
					interactionID:  "interaction-1",
					afterMessages:  len(model.state.Transcript),
					outcome:        coding.InteractionSucceeded,
					durationMillis: 1_000,
					model:          "openai/test-model",
				})
			},
			mutate: func(_ *testing.T, model *Model) {
				model.state.Transcript = append(model.state.Transcript, ai.UserText("follow up"))
				model.state.MessageCandidates = append(
					model.state.MessageCandidates, coding.CandidateIdentity{},
				)
			},
		},
		{
			name: "append a synthetic message",
			mutate: func(_ *testing.T, model *Model) {
				model.state.Transcript = append(model.state.Transcript, ai.AssistantText("hidden"))
				model.state.MessageCandidates = append(
					model.state.MessageCandidates, coding.CandidateIdentity{},
				)
				model.state.SyntheticMessages = append(
					model.state.SyntheticMessages, len(model.state.Transcript)-1,
				)
			},
		},
		{
			name: "width change",
			mutate: func(_ *testing.T, model *Model) {
				model.width = 50
				model.transcript.setLeading(0)
			},
		},
		{
			name: "theme change",
			mutate: func(_ *testing.T, model *Model) {
				model.theme = themeLight
			},
		},
		{
			name: "no color",
			mutate: func(_ *testing.T, model *Model) {
				model.options.NoColor = true
			},
		},
		{
			name: "show thinking toggled",
			mutate: func(_ *testing.T, model *Model) {
				model.controller = stubController{
					state: model.state,
					tui: config.TUIConfig{
						Screen:             config.ScreenFullscreen,
						ShowThinkingBlocks: false,
					},
				}
			},
		},
		{
			name: "session switch",
			mutate: func(_ *testing.T, model *Model) {
				model.state.SessionID = "session-2"
				model.state.Transcript = []ai.Message{ai.UserText("another session")}
				model.state.MessageCandidates = nil
			},
		},
		{
			name: "plan mode notice appended",
			mutate: func(_ *testing.T, model *Model) {
				model.queuePlanModeNotice("plan row")
			},
		},
		{
			name: "second plan mode notice appended",
			arrange: func(_ *testing.T, model *Model) {
				model.queuePlanModeNotice("plan row")
			},
			mutate: func(_ *testing.T, model *Model) {
				model.queuePlanModeNotice("second plan row")
			},
		},
		{
			name: "completion marker appended",
			mutate: func(_ *testing.T, model *Model) {
				model.completionMarkers = append(model.completionMarkers, completionMarker{
					interactionID:  "interaction-1",
					afterMessages:  len(model.state.Transcript),
					outcome:        coding.InteractionSucceeded,
					durationMillis: 7_000,
					model:          "openai/test-model",
				})
			},
		},
		{
			name: "stream error set",
			mutate: func(_ *testing.T, model *Model) {
				model.streamErr = errors.New("boom")
			},
		},
		{
			name: "stream error cleared",
			arrange: func(_ *testing.T, model *Model) {
				model.streamErr = errors.New("boom")
			},
			mutate: func(_ *testing.T, model *Model) {
				model.streamErr = nil
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()

			model := projectionCacheModel(t)
			if scenario.arrange != nil {
				scenario.arrange(t, model)
			}
			// Warm the cache against the pre-mutation model so the comparison
			// below exercises reuse, not a cold rebuild.
			require.NotEmpty(t, viewportProjectionBlocks(model))

			scenario.mutate(t, model)

			require.Equal(t, model.transcriptBlocks(), viewportProjectionBlocks(model))
			// Projecting twice must be stable: the second frame reuses the first.
			require.Equal(t, viewportProjectionBlocks(model), viewportProjectionBlocks(model))
		})
	}
}

// TestProjectionCacheRebuildsPrefixOnlyWhenInputsChange pins the two paths apart:
// a volatile draft keeps the committed prefix, a committed append extends it.
func TestProjectionCacheRebuildsPrefixOnlyWhenInputsChange(t *testing.T) {
	t.Parallel()

	model := projectionCacheModel(t)

	// Warm the cache.
	model.viewportProjection()
	prefixLength := len(model.frameCache.entries)
	require.Greater(t, prefixLength, 0)

	// A draft delta is volatile: the committed prefix is reused verbatim.
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "partial answer"})
	frame := model.viewportProjection()
	require.Equal(t, prefixLength, frame.stable)
	require.Zero(t, model.frameExtends)

	// A committed append grows the conversation: the prefix is extended, so the
	// store keeps every record the old prefix already had.
	model.state.Transcript = append(model.state.Transcript, ai.UserText("committed"))
	model.state.MessageCandidates = append(
		model.state.MessageCandidates,
		coding.CandidateIdentity{RunID: "run-1", Turn: 3},
	)
	frame = model.viewportProjection()
	require.Equal(t, 1, model.frameExtends)
	require.Equal(t, prefixLength, frame.stable)
	require.Equal(t, model.transcriptBlocks(), viewportProjectionFrameBlocks(frame))
}

// TestProjectionCacheExtendsCommittedAppends pins the append path's observable
// behaviour frame after frame: every text commit extends the cached prefix, keeps
// the records before it, and still projects exactly the pure projection.
func TestProjectionCacheExtendsCommittedAppends(t *testing.T) {
	t.Parallel()

	model := projectionCacheModel(t)
	model.viewportProjection()
	prefixLength := len(model.frameCache.entries)
	require.Greater(t, prefixLength, 0)

	for turn := 3; turn <= 5; turn++ {
		model.state.Transcript = append(
			model.state.Transcript, ai.AssistantText("answer "+strconv.Itoa(turn)),
		)
		model.state.MessageCandidates = append(
			model.state.MessageCandidates,
			coding.CandidateIdentity{RunID: "run-1", Turn: turn},
		)

		frame := model.viewportProjection()
		require.Equal(t, turn-2, model.frameExtends, "every text commit extends")
		require.Equal(t, prefixLength, frame.stable, "the store keeps the covered prefix")
		require.Equal(t, model.transcriptBlocks(), viewportProjectionFrameBlocks(frame))
		prefixLength = len(model.frameCache.entries)
	}
}

// TestProjectionCacheExtendsAcrossAPendingMarker pins the marker seam: a commit
// whose frame moves a trailing completion marker into the committed prefix still
// projects exactly the pure projection.
func TestProjectionCacheExtendsAcrossAPendingMarker(t *testing.T) {
	t.Parallel()

	model := projectionCacheModel(t)
	model.completionMarkers = []completionMarker{{
		interactionID:  "interaction-1",
		afterMessages:  len(model.state.Transcript),
		outcome:        coding.InteractionSucceeded,
		durationMillis: 7_000,
		model:          "openai/test-model",
	}}
	model.viewportProjection()
	require.Zero(t, model.frameCache.markerSplit, "the marker starts in the volatile tail")

	model.state.Transcript = append(model.state.Transcript, ai.UserText("after the marker"))
	model.state.MessageCandidates = append(model.state.MessageCandidates, coding.CandidateIdentity{})

	frame := model.viewportProjection()
	require.Equal(t, 1, model.frameExtends)
	require.Equal(t, 1, model.frameCache.markerSplit, "the marker moved into the prefix")
	require.Equal(t, model.transcriptBlocks(), viewportProjectionFrameBlocks(frame))
}

// TestProjectionCacheExtendsACommittedToolResult pins the tool append path: a
// tool result whose call the prefix had not placed yet extends the committed
// prefix instead of reprojecting it, and the store still equals the projection.
func TestProjectionCacheExtendsACommittedToolResult(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("hello"),
		ai.Assistant(
			ai.Text("reading"),
			ai.ToolCallPart{ID: "call-1", Name: "read", Args: ai.JSON(`{"path":"a.go"}`)},
		),
	}
	state.MessageCandidates = []coding.CandidateIdentity{{}, {RunID: "run-1", Turn: 1}}

	model := fullscreenModel(t, stubController{state: state}, false)
	model.state = state
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.rerenderTranscript(false)
	prefixLength := len(model.frameCache.entries)

	// The call was not placed yet, so the result extends the prefix.
	model.state.Transcript = append(
		model.state.Transcript, codingToolResultFor("call-1", "read", "contents"),
	)
	model.state.MessageCandidates = append(model.state.MessageCandidates, coding.CandidateIdentity{})
	model.rerenderTranscript(false)

	require.Equal(t, 1, model.frameExtends, "the tool result commit extends")
	require.Greater(t, len(model.frameCache.entries), prefixLength)
	require.Equal(t, model.transcriptBlocks(), storeBlocks(&model.transcript))
}

// TestProjectionCacheFallsBackWhenToolActivityTouchesACommittedRecord pins the
// tool path's fallback: a result for a call the prefix already placed would move
// that card, so the prefix is reprojected rather than extended.
//
// The store's record for that card is deliberately not asserted here. Reusing
// rendered rows by entry identity is the transcript store's own contract, and a
// committed card whose block changed under the same identity is a pre-existing
// concern that the append path neither introduces nor owns.
func TestProjectionCacheFallsBackWhenToolActivityTouchesACommittedRecord(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("hello"),
		ai.Assistant(
			ai.Text("reading"),
			ai.ToolCallPart{ID: "call-1", Name: "read", Args: ai.JSON(`{"path":"a.go"}`)},
		),
		codingToolResultFor("call-1", "read", "first contents"),
	}
	state.MessageCandidates = []coding.CandidateIdentity{{}, {RunID: "run-1", Turn: 1}, {}}

	model := fullscreenModel(t, stubController{state: state}, false)
	model.state = state
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.rerenderTranscript(false)

	// A second result for the same call names a card the prefix already holds.
	model.state.Transcript = append(
		model.state.Transcript, codingToolResultFor("call-1", "read", "second contents"),
	)
	model.state.MessageCandidates = append(model.state.MessageCandidates, coding.CandidateIdentity{})
	model.rerenderTranscript(false)

	require.Zero(t, model.frameExtends, "a restated tool result rebuilds")
	require.Equal(t, model.transcriptBlocks(), viewportProjectionBlocks(model))
}

// TestProjectionCacheFallsBackWhenCoveredPrefixChanged pins the append path's
// second fallback: a transcript whose covered range no longer matches the cached
// fingerprint is reprojected, so a rewrite inside that range cannot serve stale
// rows.
func TestProjectionCacheFallsBackWhenCoveredPrefixChanged(t *testing.T) {
	t.Parallel()

	model := projectionCacheModel(t)
	model.viewportProjection()

	replaced := append([]ai.Message(nil), model.state.Transcript...)
	replaced[0] = ai.UserText("rewritten")
	replaced = append(replaced, ai.UserText("and grown"))
	model.state.Transcript = replaced
	model.state.MessageCandidates = append(model.state.MessageCandidates, coding.CandidateIdentity{})

	frame := model.viewportProjection()
	require.Zero(t, model.frameExtends, "a rewritten covered range rebuilds")
	require.Zero(t, frame.stable)
	require.Equal(t, model.transcriptBlocks(), viewportProjectionFrameBlocks(frame))
}

// TestProjectionCacheToolLifecycle proves a running tool's progress update
// invalidates the committed prefix while an unrelated draft delta does not.
func TestProjectionCacheToolLifecycle(t *testing.T) {
	t.Parallel()

	model := projectionCacheModel(t)
	model.state.Tools = []coding.ToolState{projectionRunningReadTool()}

	// Warm the cache against the running tool.
	require.Equal(t, model.transcriptBlocks(), viewportProjectionBlocks(model))
	before := model.frameCache.stamp

	// An unrelated draft delta leaves the committed inputs untouched.
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "unrelated"})
	frame := model.viewportProjection()
	require.Equal(t, model.transcriptBlocks(), viewportProjectionFrameBlocks(frame))
	require.Greater(t, frame.stable, 0)
	require.Equal(t, before, model.frameCache.stamp)

	// The tool's progress update changes the committed fingerprint, so the
	// cached prefix is rebuilt.
	model.state.Tools[0].Update = []ai.Part{ai.Text("progress 2")}
	frame = model.viewportProjection()
	require.Equal(t, model.transcriptBlocks(), viewportProjectionFrameBlocks(frame))
	require.Zero(t, frame.stable)
	require.NotEqual(t, before, model.frameCache.stamp)
}

// TestProjectionCacheFallsBackWhenTheTranscriptWindowSlides pins the fallback at
// the Runtime's transcript cap: a commit that also drops the oldest message moved
// the covered range, so the prefix is reprojected rather than extended.
func TestProjectionCacheFallsBackWhenTheTranscriptWindowSlides(t *testing.T) {
	t.Parallel()

	model := projectionCacheModel(t)
	model.viewportProjection()

	slid := append([]ai.Message(nil), model.state.Transcript[1:]...)
	slid = append(slid, ai.UserText("pushed past the cap"), ai.UserText("and again"))
	model.state.Transcript = slid
	model.state.MessageCandidates = append(
		model.state.MessageCandidates,
		coding.CandidateIdentity{}, coding.CandidateIdentity{},
	)

	frame := model.viewportProjection()
	require.Zero(t, model.frameExtends, "a sliding window rebuilds")
	require.Zero(t, frame.stable)
	require.Equal(t, model.transcriptBlocks(), viewportProjectionFrameBlocks(frame))
}

// storeBlocks is the block list the transcript store currently holds. The store
// is what the reader sees, so a frame that kept a stale record past its seam
// shows up here even when that frame's entry list looked right.
func storeBlocks(store *transcriptStore) []timelineBlock {
	blocks := make([]timelineBlock, 0, len(store.records))
	for index := range store.records {
		blocks = append(blocks, store.records[index].block)
	}

	return blocks
}

// TestProjectionStoreKeepsUpWithTheSeam pins the record list behind `stable`:
// every frame's store must equal the pure projection, including frames where the
// live tail folded into the last committed exploration card, because the store
// keeps the records before `stable` verbatim.
func TestProjectionStoreKeepsUpWithTheSeam(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("hello"),
		ai.Assistant(
			ai.Text("reading"),
			ai.ToolCallPart{ID: "call-done", Name: "read", Args: ai.JSON(`{"path":"a.go"}`)},
		),
		codingToolResultFor("call-done", "read", "contents"),
	}
	state.MessageCandidates = []coding.CandidateIdentity{{}, {RunID: "run-1", Turn: 1}, {}}
	state.Tools = []coding.ToolState{{
		RunID: "run-1", Turn: 2,
		Call:   coding.ToolCall{ID: "call-live", Name: "read", Arguments: ai.JSON(`{"path":"b.go"}`)},
		Status: coding.ToolStatusRunning,
	}}

	model := fullscreenModel(t, stubController{state: state}, false)
	model.state = state
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Frame one folds the live exploration card into the committed one.
	model.rerenderTranscript(false)
	require.Equal(t, model.transcriptBlocks(), storeBlocks(&model.transcript), "frame one store")

	// Frame two grows the conversation: the record at the seam must be rebuilt
	// rather than kept as last frame's merged card.
	model.state.Transcript = append(model.state.Transcript, ai.UserText("more"))
	model.state.MessageCandidates = append(model.state.MessageCandidates, coding.CandidateIdentity{})
	model.rerenderTranscript(false)
	require.Equal(t, model.transcriptBlocks(), storeBlocks(&model.transcript), "frame two store")
}

// storeRows is the row text the transcript store currently holds. It is what the
// reader sees, so it catches a record the store reused past its block.
func storeRows(model *Model) string {
	rows := make([]string, 0, model.transcript.rowCount())
	for index := range model.transcript.records {
		record := &model.transcript.records[index]
		if !record.loaded {
			model.transcript.materialize(record)
		}
		rows = append(rows, record.rows.all()...)
	}

	return strings.Join(rows, "\n")
}

// toolBlockOf returns the last tool card in a block list.
func toolBlockOf(blocks []timelineBlock) timelineBlock {
	var block timelineBlock

	for _, candidate := range blocks {
		if candidate.kind == blockTool {
			block = candidate
		}
	}

	return block
}

// TestTranscriptStoreRendersACompletedToolCardAgain pins the store's reuse rule for
// a card that settles without changing its identity: with the call already
// committed, the card read "Running" while the tool ran, and the frame that carries
// the result must render it again instead of keeping the earlier row.
func TestTranscriptStoreRendersACompletedToolCardAgain(t *testing.T) {
	t.Parallel()

	call := coding.ToolCall{ID: "call-sh", Name: "shell", Arguments: ai.JSON(`{"command":"echo hi"}`)}

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("hello"),
		ai.Assistant(ai.Text("running"), ai.ToolCallPart{ID: call.ID, Name: call.Name, Args: call.Arguments}),
	}
	state.MessageCandidates = []coding.CandidateIdentity{{}, {RunID: "run-1", Turn: 1}}
	state.Tools = []coding.ToolState{{RunID: "run-1", Turn: 2, Call: call, Status: coding.ToolStatusRunning}}

	model := fullscreenModel(t, stubController{state: state}, false)
	model.state = state
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.rerenderTranscript(false)
	require.Contains(t, storeRows(model), "Running")

	model.state.Transcript = append(
		model.state.Transcript, codingToolResultFor(call.ID, call.Name, "hi"),
	)
	model.state.MessageCandidates = append(model.state.MessageCandidates, coding.CandidateIdentity{})
	model.state.Tools[0].Status = coding.ToolStatusCompleted
	model.state.Tools[0].Result = ai.ToolMessage{Parts: []ai.ToolResultPart{{
		ToolCallID: call.ID, Name: call.Name, Content: []ai.Part{ai.Text("hi")},
	}}}
	model.rerenderTranscript(false)

	require.NotContains(t, storeRows(model), "Running", "the settled card is rendered again")
	require.Equal(t, model.transcriptBlocks(), storeBlocks(&model.transcript))
}

// TestTranscriptStoreKeepsUpWithALiveResultBecomingDurable pins what keeps a card
// live: a card that reads a live-only result is terminal but not final, so the
// frame that later carries the durable result must be rendered rather than served
// from the record the live result produced.
func TestTranscriptStoreKeepsUpWithALiveResultBecomingDurable(t *testing.T) {
	t.Parallel()

	call := coding.ToolCall{ID: "call-sh", Name: "shell", Arguments: ai.JSON(`{"command":"echo hi"}`)}

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("hello"),
		ai.Assistant(ai.Text("running"), ai.ToolCallPart{ID: call.ID, Name: call.Name, Args: call.Arguments}),
	}
	state.MessageCandidates = []coding.CandidateIdentity{{}, {RunID: "run-1", Turn: 1}}
	state.Tools = []coding.ToolState{{
		RunID: "run-1", Turn: 2, Call: call, Status: coding.ToolStatusRunning,
		// Only the live activity has a result so far.
		Result: ai.ToolMessage{Parts: []ai.ToolResultPart{{
			ToolCallID: call.ID, Name: call.Name, Content: []ai.Part{ai.Text("live output")},
		}}},
	}}

	model := fullscreenModel(t, stubController{state: state}, false)
	model.state = state
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Step one: the tool is running.
	model.rerenderTranscript(false)
	require.True(t, blockIsUnsettled(toolBlockOf(model.transcriptBlocks())))

	// Step two: the live activity finished, but its own result is all there is, so
	// the card reads terminal while its durable text is still to come.
	model.state.Tools[0].Status = coding.ToolStatusCompleted
	model.rerenderTranscript(false)
	require.True(t, blockIsUnsettled(toolBlockOf(model.transcriptBlocks())),
		"a card reading a live-only result is not final")

	// Step three: the durable result lands and the live activity is gone.
	model.state.Transcript = append(
		model.state.Transcript, codingToolResultFor(call.ID, call.Name, "durable output"),
	)
	model.state.MessageCandidates = append(model.state.MessageCandidates, coding.CandidateIdentity{})
	model.state.Tools = nil
	model.rerenderTranscript(false)

	require.False(t, blockIsUnsettled(toolBlockOf(model.transcriptBlocks())))
	require.Equal(t, model.transcriptBlocks(), storeBlocks(&model.transcript),
		"the frame carrying the durable result is rendered, not reused")
}

// TestProjectionLeavesMessagePartsUntouched pins the invariant behind reading
// message parts in place: the projection only reads them, so projecting a
// conversation must leave every message exactly as it found it. A writer in this
// path would corrupt the State the Runtime also owns.
func TestProjectionLeavesMessagePartsUntouched(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("hello"),
		ai.Assistant(ai.Text("hi"), ai.ReasoningPart{Text: "reasoned"}),
		codingToolResultFor("call-1", "read", "file contents"),
		ai.Assistant(ai.Text("done"), ai.ToolCallPart{
			ID: "call-2", Name: "read", Args: ai.JSON(`{"path":"main.go"}`),
		}),
	}
	state.MessageCandidates = []coding.CandidateIdentity{
		{RunID: "run-1", Turn: 1},
		{RunID: "run-1", Turn: 2},
	}
	state.Tools = []coding.ToolState{projectionRunningReadTool()}

	before := make(ai.Messages, len(state.Transcript))
	for index, message := range state.Transcript {
		cloned, err := ai.CloneMessage(message)
		require.NoError(t, err)

		before[index] = cloned
	}

	model := projectionCacheModel(t)
	model.state = state

	require.NotEmpty(t, viewportProjectionBlocks(model), "the cached frame projects")
	require.NotEmpty(t, model.transcriptBlocks(), "the pure projection projects")

	require.Equal(t, before, state.Transcript, "the projection must only read the conversation")
}
