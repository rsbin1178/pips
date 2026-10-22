package coding

import (
	"reflect"

	"github.com/rsbin1178/pips/ai"
)

// replaceMessageCandidates carries ownership across a durable refresh of the
// same branch. The durable store can insert deferred tool results ahead of
// messages already committed to the live projection. Compare the retained
// suffix in order, rather than searching by content: identical answers in
// different turns must never exchange their run identities.
func (state *State) replaceMessageCandidates(transcript ai.Messages) {
	candidates := make([]CandidateIdentity, len(transcript))
	if len(state.MessageCandidates) != len(state.Transcript) {
		state.MessageCandidates = candidates

		return
	}

	previous, next := len(state.Transcript)-1, len(transcript)-1
	for previous >= 0 && next >= 0 {
		if reflect.DeepEqual(state.Transcript[previous], transcript[next]) {
			candidates[next] = state.MessageCandidates[previous]
			previous--
			next--

			continue
		}

		if _, insertedResult := transcript[next].(ai.ToolMessage); insertedResult {
			next--

			continue
		}

		// A changed non-tool message is a replacement, not a deferred result.
		// Do not let a coincidentally identical suffix inherit the old owner.
		clear(candidates)

		break
	}

	state.MessageCandidates = candidates
}
