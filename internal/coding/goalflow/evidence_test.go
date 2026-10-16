package goalflow

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
)

// TestEvidenceSupersedesOnlyTheExactToolInvocation pins the merge rule the proof
// rules lean on: a successful rerun supersedes the exact failed invocation, and a
// file read can never erase failed test evidence. The machine proof gate that used
// to read this is gone (Phase AA); the merge it depended on is unchanged.
func TestEvidenceSupersedesOnlyTheExactToolInvocation(t *testing.T) {
	t.Parallel()

	failed := Record{ID: "tool:failed", Tool: "shell", Arguments: ai.JSON(`{"command":"go test ./..."}`), Digest: "failure", OK: false}
	file := Record{ID: "tool:file", Tool: "read", Arguments: ai.JSON(`{"path":"main.go"}`), Digest: "file", OK: true}
	evidence := Merge(Evidence{Records: []Record{failed}}, Evidence{Records: []Record{file}})
	assert.Len(t, evidence.Records, 2, "a file read cannot erase failed test evidence")

	corrected := failed
	corrected.ID = "tool:successful-rerun"
	corrected.Digest = "success"
	corrected.OK = true
	evidence = Merge(evidence, Evidence{Records: []Record{corrected}})
	assert.Len(t, evidence.Records, 2)

	for _, record := range evidence.Records {
		assert.NotEqual(t, failed.ID, record.ID)
	}
}

func TestEvidenceSubstanceIgnoresExecutionMetadata(t *testing.T) {
	t.Parallel()

	first := Evidence{Records: []Record{{Tool: "shell", Digest: "full-result-duration-10", Substance: "same-output"}}}
	second := Evidence{Records: []Record{{Tool: "shell", Digest: "full-result-duration-20", Substance: "same-output"}}}
	assert.Equal(t, first.Fingerprint(), second.Fingerprint())
	second.Records[0].Substance = "different-output"
	assert.NotEqual(t, first.Fingerprint(), second.Fingerprint())
}
