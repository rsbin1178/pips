//nolint:wsl_v5 // Child lifecycle fixtures keep model steps and assertions together.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/compaction"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type checkpointModel struct {
	mu        sync.Mutex
	normal    func(context.Context, ai.Request, int) (*ai.Response, error)
	summary   func(context.Context, ai.Request) (*ai.Response, error)
	requests  []ai.Request
	summaries []ai.Request
}

func (m *checkpointModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	isSummary := request.ResponseFormat == nil
	index := len(m.requests)
	if isSummary {
		m.summaries = append(m.summaries, request)
	} else {
		m.requests = append(m.requests, request)
	}
	m.mu.Unlock()
	if isSummary {
		if m.summary != nil {
			return m.summary(ctx, request)
		}
		return responseText("A concise continuation summary without the omitted file details."), nil
	}
	return m.normal(ctx, request, index)
}

func (*checkpointModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errors.New("unexpected streaming request in blocking Manager"))
	}
}

func (*checkpointModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*checkpointModel) ModelID() string       { return "child-checkpoint-test" }

func (*checkpointModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}

func (m *checkpointModel) snapshots() ([]ai.Request, []ai.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests), slices.Clone(m.summaries)
}

func childCheckpointPolicy() compaction.Policy {
	return compaction.Policy{
		ContextWindow: 12000, ReserveTokens: 1000, MaxOutputTokens: 256,
		MinSummaryChars: 1, AttemptsPerStage: 1, Timeout: 30 * time.Second,
	}
}

func childCheckpointFixture(t *testing.T, model *checkpointModel, configure func(*Config)) managerFixture {
	t.Helper()
	limits := DefaultLimits()
	limits.MaxOutputTokens = 256
	fixture := newManagerFixtureWithConfig(t, model, ExecutionOptions{Limits: limits}, func(config *Config) {
		policy := childCheckpointPolicy()
		config.FullCompaction = &policy
		if configure != nil {
			configure(config)
		}
	})
	require.NoError(t, os.WriteFile(filepath.Join(fixture.root, "large.txt"), []byte("omitted-detail: child evidence "+strings.Repeat("evidence ", 12000)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fixture.root, "tiny.txt"), []byte("tiny"), 0o600))
	return fixture
}

func checkpointReadCall(id, path string) *ai.Response {
	args, _ := json.Marshal(map[string]string{"path": path})
	return responseToolCall(ai.ToolCallPart{ID: id, Name: readToolName, Args: args})
}

func checkpointHistoryCall(id string, args any) *ai.Response {
	encoded, _ := json.Marshal(args)
	return responseToolCall(ai.ToolCallPart{ID: id, Name: tools.HistoryName, Args: encoded})
}

func checkpointFinalResponse() *ai.Response {
	return responseText(`{"summary":"done","evidence":[],"unknowns":[]}`)
}

func lastCheckpointToolResult(request ai.Request) (ai.ToolResultPart, error) {
	for _, message := range slices.Backward(request.Messages) {
		if tool, ok := message.(ai.ToolMessage); ok && len(tool.Parts) > 0 {
			return tool.Parts[len(tool.Parts)-1], nil
		}
	}
	return ai.ToolResultPart{}, errors.New("missing tool result")
}

func checkpointEntries(path []harness.Entry) []harness.Entry {
	var entries []harness.Entry
	for _, entry := range path {
		if entry.Kind == harness.KindContextCheckpoint {
			entries = append(entries, entry)
		}
	}
	return entries
}

func TestManagerFullCheckpointAndOwnHistoryRead(t *testing.T) {
	t.Parallel()
	model := &checkpointModel{}
	archiveID := ""
	model.normal = func(_ context.Context, request ai.Request, index int) (*ai.Response, error) {
		switch index {
		case 0:
			return checkpointReadCall("read-large", "large.txt"), nil
		case 1:
			return checkpointHistoryCall("history-list", map[string]any{"action": "list"}), nil
		case 2:
			result, err := lastCheckpointToolResult(request)
			if err != nil {
				return nil, err
			}
			var page session.ArchiveListPage
			if err := json.Unmarshal([]byte(partsText(result.Content)), &page); err != nil {
				return nil, err
			}
			if len(page.Archives) != 1 {
				return nil, fmt.Errorf("expected one child archive, got %d", len(page.Archives))
			}
			archiveID = page.Archives[0].ID
			return checkpointHistoryCall("history-search", map[string]any{
				"action": "search", "archive_id": page.Archives[0].ID, "query": "omitted-detail",
			}), nil
		case 3:
			result, err := lastCheckpointToolResult(request)
			if err != nil {
				return nil, err
			}
			var page session.ArchiveSearchPage
			if err := json.Unmarshal([]byte(partsText(result.Content)), &page); err != nil {
				return nil, err
			}
			if len(page.Matches) == 0 {
				return nil, errors.New("archive lost omitted evidence")
			}
			match := page.Matches[0]
			return checkpointHistoryCall("history-read", map[string]any{
				"action": "read", "archive_id": archiveID, "segment_id": match.SegmentID,
				"offset": match.Line - 1, "limit": 1,
			}), nil
		case 4:
			return checkpointFinalResponse(), nil
		default:
			return nil, errors.New("unexpected extra normal request")
		}
	}
	fixture := childCheckpointFixture(t, model, func(config *Config) {
		// The legacy fields must be ignored when full mode is selected.
		config.Compaction = &harness.CompactionSettings{}
		config.SummaryModel = &testModel{}
		config.RequestPolicy = func(request *ai.Request) {
			request.ResponseFormat = &ai.ResponseFormat{Name: "must-not-reach-summary", Schema: &ai.Schema{Type: "object"}}
		}
	})
	execution, err := fixture.manager.Start(t.Context(), Request{Role: RoleExplore, Task: "Read the file and recover exact omitted details from your history."}, nil)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)
	requests, summaries := model.snapshots()
	require.Len(t, requests, 5)
	require.Len(t, summaries, 1)
	assert.Empty(t, summaries[0].Tools)
	assert.Nil(t, summaries[0].ResponseFormat)
	assert.Contains(t, requestText(requests[1]), "Historical context checkpoint")
	assert.NotContains(t, requestText(requests[1]), "omitted-detail: child evidence")
	assert.Contains(t, toolNames(requests[0].Tools), tools.HistoryName)
	readResult, err := lastCheckpointToolResult(requests[4])
	require.NoError(t, err)
	assert.False(t, readResult.IsError)
	assert.Contains(t, partsText(readResult.Content), "omitted-detail: child evidence")
	entries := checkpointEntries(execution.child.Session().Path())
	require.Len(t, entries, 1)
	parent, ok := execution.child.Session().Entry(entries[0].ParentID)
	require.True(t, ok)
	assert.IsType(t, ai.ToolMessage{}, parent.Message, "the producing tool result must already be durable")
	fixed, err := compaction.RequestOverheadTokens(requests[0])
	require.NoError(t, err)
	assert.Positive(t, fixed)
	assert.Equal(t, fixed, entries[0].Checkpoint.FixedTokens)
	assert.Zero(t, countChildCompactions(execution.child.Session().Path()))
	assert.Contains(t, execution.plan.Capabilities, EffectiveCapability{WireName: tools.HistoryName, Source: "builtin:session", Risk: "read"})
}

func TestManagerFullCheckpointSummaryFailureStopsNextRequest(t *testing.T) {
	t.Parallel()
	model := &checkpointModel{
		normal: func(context.Context, ai.Request, int) (*ai.Response, error) {
			return checkpointReadCall("read-large", "large.txt"), nil
		},
		summary: func(context.Context, ai.Request) (*ai.Response, error) {
			return responseText("<think>private analysis only</think>"), nil
		},
	}
	fixture := childCheckpointFixture(t, model, nil)
	execution, err := fixture.manager.Start(t.Context(), Request{Role: RoleExplore, Task: "Read the file."}, nil)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.Error(t, err)
	assert.Equal(t, OutcomeFailed, result.Outcome)
	requests, summaries := model.snapshots()
	assert.Len(t, requests, 1)
	assert.Len(t, summaries, 1)
	assert.Empty(t, checkpointEntries(execution.child.Session().Path()))
}

func TestManagerFullCheckpointCancellation(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	model := &checkpointModel{
		normal: func(context.Context, ai.Request, int) (*ai.Response, error) {
			return checkpointReadCall("read-large", "large.txt"), nil
		},
		summary: func(ctx context.Context, _ ai.Request) (*ai.Response, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	fixture := childCheckpointFixture(t, model, nil)
	execution, err := fixture.manager.Start(t.Context(), Request{Role: RoleExplore, Task: "Read the file."}, nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}, 30*time.Second, 10*time.Millisecond)
	execution.Cancel()
	result, err := execution.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, OutcomeCanceled, result.Outcome)
	requests, _ := model.snapshots()
	assert.Len(t, requests, 1)
	assert.Empty(t, checkpointEntries(execution.child.Session().Path()))
}

func TestManagerHistoryNeverUsesParentOrSiblingArchives(t *testing.T) {
	t.Parallel()
	firstArchive := ""
	model := &checkpointModel{}
	model.normal = func(_ context.Context, request ai.Request, index int) (*ai.Response, error) {
		switch index {
		case 0:
			return checkpointReadCall("child-one-read", "large.txt"), nil
		case 1:
			return checkpointHistoryCall("child-one-list", map[string]any{"action": "list"}), nil
		case 2:
			result, err := lastCheckpointToolResult(request)
			if err != nil {
				return nil, err
			}
			var page session.ArchiveListPage
			if err := json.Unmarshal([]byte(partsText(result.Content)), &page); err != nil {
				return nil, err
			}
			if len(page.Archives) != 1 {
				return nil, errors.New("first child saw archives outside its checkpoint path")
			}
			firstArchive = page.Archives[0].ID
			return checkpointFinalResponse(), nil
		case 3:
			return checkpointHistoryCall("child-two-list", map[string]any{"action": "list"}), nil
		case 4:
			result, err := lastCheckpointToolResult(request)
			if err != nil {
				return nil, err
			}
			var page session.ArchiveListPage
			if err := json.Unmarshal([]byte(partsText(result.Content)), &page); err != nil {
				return nil, err
			}
			if len(page.Archives) != 0 {
				return nil, errors.New("new child inherited another child's archives")
			}
			return checkpointHistoryCall("child-two-foreign-read", map[string]any{
				"action": "read", "archive_id": firstArchive, "segment_id": "seg-000001", "limit": 1,
			}), nil
		case 5:
			return checkpointFinalResponse(), nil
		default:
			return nil, errors.New("unexpected child request")
		}
	}
	fixture := childCheckpointFixture(t, model, nil)
	parentTip, err := fixture.parent.Session().AppendMessage(ai.UserText("parent-private-evidence"), nil)
	require.NoError(t, err)
	parentStore, err := fixture.repository.Archives(fixture.parent)
	require.NoError(t, err)
	staged, err := parentStore.Stage(t.Context(), session.ArchiveSource{
		SessionID: fixture.parent.Metadata().ID, TipID: parentTip,
	}, fixture.parent.Session().Path())
	require.NoError(t, err)
	require.NoError(t, staged.Publish(t.Context()))
	parentArchive := staged.ID()
	require.NoError(t, staged.Close())
	_, err = fixture.parent.Session().AppendContextCheckpoint(parentTip, harness.ContextCheckpoint{
		Version: harness.ContextCheckpointVersion, ArchiveID: parentArchive,
		Messages: ai.Messages{ai.UserText("parent checkpoint")},
	}, 100)
	require.NoError(t, err)
	first, err := fixture.manager.Start(t.Context(), Request{Role: RoleExplore, Task: "First child assignment."}, nil)
	require.NoError(t, err)
	_, err = first.Wait(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, firstArchive)
	assert.NotEqual(t, parentArchive, firstArchive)
	second, err := fixture.manager.Start(t.Context(), Request{Role: RoleExplore, Task: "Second child assignment."}, nil)
	require.NoError(t, err)
	_, err = second.Wait(t.Context())
	require.NoError(t, err)
	requests, summaries := model.snapshots()
	require.Len(t, requests, 6)
	assert.Len(t, summaries, 1)
	foreign, err := lastCheckpointToolResult(requests[5])
	require.NoError(t, err)
	assert.True(t, foreign.IsError)
	assert.NotContains(t, partsText(foreign.Content), "omitted-detail: child evidence")
	assert.Empty(t, checkpointEntries(second.child.Session().Path()))
}

func TestManagerFullCheckpointFinalizationKeepsHistoryDisabled(t *testing.T) {
	t.Parallel()
	model := &checkpointModel{normal: func(_ context.Context, _ ai.Request, index int) (*ai.Response, error) {
		switch index {
		case 0:
			return checkpointReadCall("large", "large.txt"), nil
		case 1:
			return checkpointReadCall("tiny", "tiny.txt"), nil
		case 2:
			return checkpointFinalResponse(), nil
		default:
			return nil, errors.New("finalization did not stop child")
		}
	}}
	fixture := childCheckpointFixture(t, model, func(config *Config) {
		config.Options.Limits.MaxTurns = 3
		config.Options.Limits.FinalizationTurns = 1
		config.RequestPolicy = func(request *ai.Request) {
			if len(request.Tools) == 0 {
				request.Tools = []ai.Tool{tools.NewHistoryTool(nil).Decl()}
			}
		}
	})
	execution, err := fixture.manager.Start(t.Context(), Request{Role: RoleExplore, Task: "Read evidence then finalize."}, nil)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, result.Outcome)
	requests, summaries := model.snapshots()
	require.Len(t, requests, 3)
	assert.Contains(t, toolNames(requests[0].Tools), tools.HistoryName)
	assert.Empty(t, requests[2].Tools, "request policy cannot restore tools after finalization")
	assert.NotNil(t, requests[2].ResponseFormat, "normal final output schema survives")
	assert.Contains(t, requestText(requests[2]), finalizationInstruction)
	assert.Len(t, summaries, 1)
	assert.Len(t, checkpointEntries(execution.child.Session().Path()), 1)
}

func TestManagerFullCheckpointRequiresAuthorizedHistory(t *testing.T) {
	t.Parallel()
	model := &checkpointModel{normal: func(context.Context, ai.Request, int) (*ai.Response, error) {
		return checkpointReadCall("read-large", "large.txt"), nil
	}}
	fixture := childCheckpointFixture(t, model, func(config *Config) {
		config.RequestPolicy = func(request *ai.Request) {
			request.Tools = slices.DeleteFunc(request.Tools, func(tool ai.Tool) bool { return tool.Name == tools.HistoryName })
		}
	})
	execution, err := fixture.manager.Start(t.Context(), Request{Role: RoleExplore, Task: "Read the file."}, nil)
	require.NoError(t, err)
	result, err := execution.Wait(t.Context())
	require.ErrorContains(t, err, "requires authorized session_history access")
	assert.Equal(t, OutcomeFailed, result.Outcome)
	requests, summaries := model.snapshots()
	assert.Len(t, requests, 1)
	assert.Empty(t, summaries)
	assert.Empty(t, checkpointEntries(execution.child.Session().Path()))
}
