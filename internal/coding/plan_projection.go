package coding

import (
	"slices"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/planreview"
)

const maxPlanProposals = 128

func planProposalFromRequest(request planreview.Request) (PlanProposal, bool) {
	if request.Content == "" || planreview.ValidateRequest(request) != nil {
		return PlanProposal{}, false
	}

	return PlanProposal{
		ID: request.ID, ToolCallID: request.ToolCallID, Revision: request.Revision,
		Size: request.Size, Content: request.Content, Status: PlanProposalPending,
	}, true
}

func upsertPlanProposal(values []PlanProposal, proposal PlanProposal) []PlanProposal {
	values = slices.Clone(values)
	if index := slices.IndexFunc(values, func(value PlanProposal) bool {
		return value.ID == proposal.ID
	}); index >= 0 {
		values[index] = proposal

		return values
	}

	values = append(values, proposal)
	if len(values) > maxPlanProposals {
		values = slices.Clone(values[len(values)-maxPlanProposals:])
	}

	return values
}

func resolvePlanProposal(
	values []PlanProposal,
	requestID string,
	revision string,
	decision planreview.Decision,
) []PlanProposal {
	values = slices.Clone(values)
	for index := range values {
		if values[index].ID != requestID || values[index].Revision != revision {
			continue
		}

		if decision == planreview.DecisionApprove {
			values[index].Status = PlanProposalApproved
		} else {
			values[index].Status = PlanProposalContinued
		}
	}

	return values
}

// projectPlanProposals is the single durable transcript decoder used during
// bootstrap. Frontends consume its typed result and never decode Tool JSON.
func projectPlanProposals(transcript []ai.Message) []PlanProposal {
	proposals := make([]PlanProposal, 0)
	byCall := make(map[string]string)

	for _, message := range transcript {
		for _, part := range message.Parts {
			switch value := part.(type) {
			case ai.ToolCallPart:
				request, err := planreview.ProposalFromCall(value)
				if err != nil {
					continue
				}

				proposal, ok := planProposalFromRequest(request)
				if !ok {
					continue
				}

				proposals = upsertPlanProposal(proposals, proposal)
				byCall[value.ID] = proposal.ID
			case ai.ToolResultPart:
				proposalID, ok := byCall[value.ToolCallID]
				if !ok {
					continue
				}

				decision, ok := planreview.DecisionFromResult(value)
				if !ok {
					continue
				}

				for index := range proposals {
					if proposals[index].ID != proposalID {
						continue
					}

					proposals = resolvePlanProposal(
						proposals, proposalID, proposals[index].Revision, decision,
					)

					break
				}
			}
		}
	}

	return proposals
}
