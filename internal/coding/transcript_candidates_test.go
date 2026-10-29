package coding

import (
	"slices"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTreeRefreshPreservesCandidateOwnership(t *testing.T) {
	t.Parallel()

	call := ai.Assistant(ai.ToolCallPart{ID: "question", Name: "ask_user", Args: ai.JSON(`{}`)})
	result := ai.ToolResultText("question", "ask_user", "ok")
	question, answer := ai.UserText("repeat"), ai.AssistantText("Done.")
	first, second := CandidateIdentity{RunID: "first", Turn: 1}, CandidateIdentity{RunID: "second", Turn: 1}

	for _, test := range []struct {
		name       string
		transcript ai.Messages
		base       int
		want       []CandidateIdentity
	}{
		{
			name:       "deferred result between identical answers",
			transcript: ai.Messages{question, answer, question, call, result, answer},
			want:       []CandidateIdentity{{}, first, {}, second, {}, second},
		},
		{
			name:       "insertion also advances bounded window base",
			transcript: ai.Messages{question, call, result, answer}, base: 12,
			want: []CandidateIdentity{{}, second, {}, second},
		},
		{
			name:       "same length replacement with identical final answer",
			transcript: ai.Messages{ai.UserText("another branch"), answer, question, call, answer},
			want:       make([]CandidateIdentity, 5),
		},
		{
			name: "empty replacement",
			want: []CandidateIdentity{},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			state := State{
				SessionID: "session-1", SessionOpen: true, Sequence: 1,
				Transcript:        ai.Messages{question, answer, question, call, answer},
				MessageCandidates: []CandidateIdentity{{}, first, {}, second, second},
				transcriptBase:    10,
			}
			retained := slices.Clone(state.MessageCandidates)
			event := Event{
				Schema: EventSchema, Sequence: 2, Time: time.Unix(1, 0).UTC(),
				SessionID: "session-1", Type: EventSessionTreeChanged,
				Payload: SessionTreeChanged{
					Tree:       SessionTree{SessionID: "session-1"},
					Transcript: test.transcript, TranscriptBase: test.base,
				},
			}
			next, err := Reduce(state, event)
			require.NoError(t, err)
			assert.Equal(t, retained, state.MessageCandidates, "refresh must not modify retained snapshots")
			require.Len(t, next.MessageCandidates, len(next.Transcript))
			assert.Equal(t, test.want, next.MessageCandidates)
			assert.Equal(t, test.base, next.TranscriptBase())

			sibling, err := Reduce(state, event)
			require.NoError(t, err)

			if len(next.MessageCandidates) > 0 {
				next.MessageCandidates[len(next.MessageCandidates)-1] = CandidateIdentity{RunID: "mutated"}
				assert.Equal(t, test.want, sibling.MessageCandidates, "sibling reductions own their candidate arrays")
			}
		})
	}
}

func TestNavigationDoesNotTransferOwnershipToIdenticalBranch(t *testing.T) {
	t.Parallel()

	state := State{
		SessionID: "session-1", SessionOpen: true, Sequence: 1, Phase: PhaseIdle,
		Transcript:        ai.Messages{ai.UserText("repeat"), ai.AssistantText("Done.")},
		MessageCandidates: []CandidateIdentity{{}, {RunID: "old-branch", Turn: 1}},
	}
	navigation := Event{
		Schema: EventSchema, Sequence: 2, Time: time.Unix(1, 0).UTC(),
		SessionID: "session-1", Type: EventSessionNavigated,
		Payload: SessionNavigated{FromID: "old-leaf", ToID: "new-leaf"},
	}
	refresh := Event{
		Schema: EventSchema, Sequence: 3, Time: time.Unix(1, 0).UTC(),
		SessionID: "session-1", Type: EventSessionTreeChanged,
		Payload: SessionTreeChanged{Tree: SessionTree{SessionID: "session-1"}, Transcript: state.Transcript},
	}
	next, err := ReduceBatch(state, []Event{navigation, refresh})
	require.NoError(t, err)
	assert.Equal(t, []CandidateIdentity{{}, {}}, next.MessageCandidates)
	assert.Equal(t, "old-branch", state.MessageCandidates[1].RunID)
}
