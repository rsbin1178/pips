//nolint:wsl_v5 // Winner selection and diagnostic normalization stay adjacent.
package resource

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rsbin/pips/agent/harness"
)

func resolveSkills(
	candidates []skillEntry,
	diagnostics []Diagnostic,
) ([]harness.Skill, []Diagnostic, error) {
	merged, diagnostics, err := mergeSkillEntries(candidates, slices.Clone(diagnostics))
	if err != nil {
		return nil, nil, err
	}

	result := make([]harness.Skill, 0, len(merged))
	for _, entry := range merged {
		result = append(result, cloneSkill(entry.skill))
	}

	if _, err := harness.NewSkillCatalog(result...); err != nil {
		return nil, nil, fmt.Errorf("%w: validate resolved Skills: %w", ErrInvalid, err)
	}

	return result, diagnostics, nil
}

func mergeSkillEntries(
	candidates []skillEntry,
	diagnostics []Diagnostic,
) ([]skillEntry, []Diagnostic, error) {
	byName := make(map[string]skillEntry, len(candidates))

	for _, candidate := range candidates {
		current, exists := byName[candidate.skill.Name]
		if !exists {
			byName[candidate.skill.Name] = candidate
			continue
		}

		if current.priority == candidate.priority {
			if !current.direct || !candidate.direct {
				return nil, nil, fmt.Errorf(
					"%w: skill %q from %q and %q",
					ErrDuplicate,
					candidate.skill.Name,
					current.provenance,
					candidate.provenance,
				)
			}

			winner, suppressed := current, candidate
			if strings.Compare(candidate.provenance, current.provenance) < 0 {
				winner, suppressed = candidate, current
				byName[candidate.skill.Name] = candidate
			}

			diagnostics = append(diagnostics, Diagnostic{
				Code:       "skill_suppressed",
				Resource:   candidate.skill.Name,
				Winner:     winner.provenance,
				Suppressed: suppressed.provenance,
				Message:    "same-precedence Skill was deterministically suppressed",
			})

			continue
		}

		winner, suppressed := current, candidate
		if candidate.priority > current.priority {
			winner, suppressed = candidate, current
			byName[candidate.skill.Name] = candidate
		}

		diagnostics = append(diagnostics, Diagnostic{
			Code:       "skill_suppressed",
			Resource:   candidate.skill.Name,
			Winner:     winner.provenance,
			Suppressed: suppressed.provenance,
			Message:    "lower-precedence Skill was suppressed",
		})
	}

	result := make([]skillEntry, 0, len(byName))
	for _, entry := range byName {
		result = append(result, entry)
	}
	for index := range diagnostics {
		if diagnostics[index].Code != "skill_suppressed" {
			continue
		}

		winner, ok := byName[diagnostics[index].Resource]
		if ok {
			diagnostics[index].Winner = winner.provenance
		}
	}

	slices.SortFunc(result, func(left, right skillEntry) int {
		return strings.Compare(left.skill.Name, right.skill.Name)
	})
	slices.SortFunc(diagnostics, compareDiagnostic)

	return result, diagnostics, nil
}

func compareDiagnostic(left, right Diagnostic) int {
	if compared := strings.Compare(left.Code, right.Code); compared != 0 {
		return compared
	}

	if compared := strings.Compare(left.Resource, right.Resource); compared != 0 {
		return compared
	}

	if compared := strings.Compare(left.Winner, right.Winner); compared != 0 {
		return compared
	}

	return strings.Compare(left.Suppressed, right.Suppressed)
}
