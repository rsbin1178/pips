package coding

import (
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectPlanProposalsReconstructsFullResolvedPlan(t *testing.T) {
	t.Parallel()

	const content = "# Durable Plan\n\nRestore this exact content after restart."

	transcript := []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "present-1", Name: planreview.PresentToolName,
			Args: ai.JSON(fmt.Sprintf(`{"expected_revision":"","content":%q}`, content)),
		}),
		ai.ToolResultText("present-1", planreview.PresentToolName, planreview.ApprovalToolResult),
	}

	proposals := projectPlanProposals(transcript)
	require.Len(t, proposals, 1)
	assert.Equal(t, "present-1", proposals[0].ToolCallID)
	assert.Equal(t, content, proposals[0].Content)
	assert.Equal(t, PlanProposalApproved, proposals[0].Status)
}

func TestProjectPlanProposalsIgnoresMalformedOrUnownedResults(t *testing.T) {
	t.Parallel()

	transcript := []ai.Message{
		ai.Assistant(ai.ToolCallPart{
			ID: "malformed", Name: planreview.PresentToolName,
			Args: ai.JSON(`{"content":123}`),
		}),
		ai.ToolResultText("unknown", planreview.PresentToolName, planreview.ApprovalToolResult),
	}

	assert.Empty(t, projectPlanProposals(transcript))
}
