package coding

import (
	"slices"
	"strconv"
	"strings"

	"github.com/rsbin1178/pips/agent/catalog"
)

const (
	// toolSuggestionLimit bounds how many close names an unknown-tool denial
	// offers the model.
	toolSuggestionLimit = 3
	// toolSuggestionMinJaccard accepts a candidate whose stemmed name tokens
	// overlap at least half with the requested name.
	toolSuggestionMinJaccard = 0.5
	// toolSuggestionMaxEdits accepts a candidate within two single-character
	// edits or adjacent transpositions of the requested name.
	toolSuggestionMaxEdits = 2
)

// suggestToolNames returns up to limit known names closest to name, best
// first. A candidate qualifies by token overlap (so "search_tools" finds
// "pips_tool_search") or by a small edit distance on the raw lowercase name
// (so "raed" finds "read"). Ties break alphabetically.
func suggestToolNames(name string, known []string, limit int) []string {
	if limit <= 0 || name == "" {
		return nil
	}

	type scored struct {
		name  string
		score float64
	}

	requested := tokenSet(catalog.SearchTokens(name))
	lowered := strings.ToLower(name)

	candidates := make([]scored, 0, len(known))
	for _, candidate := range known {
		if candidate == "" || candidate == name {
			continue
		}

		score := jaccard(requested, tokenSet(catalog.SearchTokens(candidate)))
		if score < toolSuggestionMinJaccard {
			loweredCandidate := strings.ToLower(candidate)
			longer := max(len(lowered), len(loweredCandidate))
			distance := editDistance(lowered, loweredCandidate)
			// Two edits on a two-character name is a different name, not a
			// typo, so the edit budget is also bounded by half the length.
			if distance > toolSuggestionMaxEdits || distance*2 > longer {
				continue
			}

			score = 1 - float64(distance)/float64(longer)
		}

		candidates = append(candidates, scored{name: candidate, score: score})
	}

	slices.SortStableFunc(candidates, func(a, b scored) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		default:
			return strings.Compare(a.name, b.name)
		}
	})

	if len(candidates) == 0 {
		return nil
	}

	names := make([]string, 0, min(limit, len(candidates)))
	for _, candidate := range candidates[:min(limit, len(candidates))] {
		names = append(names, candidate.name)
	}

	return names
}

// unknownToolReason renders the denial for a name outside the toolbox,
// appending the closest known names when any qualify. Names are quoted with
// strconv.Quote so a model-supplied name cannot smuggle control characters or
// quotes into the one-line denial.
func unknownToolReason(name string, known []string) string {
	reason := "unknown tool " + strconv.Quote(name)

	suggestions := suggestToolNames(name, known, toolSuggestionLimit)
	if len(suggestions) == 0 {
		return reason
	}

	quoted := make([]string, len(suggestions))
	for index, suggestion := range suggestions {
		quoted[index] = strconv.Quote(suggestion)
	}

	return reason + "; did you mean " + strings.Join(quoted, ", ") + "?"
}

func tokenSet(tokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		set[token] = struct{}{}
	}

	return set
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}

	intersection := 0

	for token := range a {
		if _, ok := b[token]; ok {
			intersection++
		}
	}

	union := len(a) + len(b) - intersection

	return float64(intersection) / float64(union)
}

// editDistance is the optimal string alignment distance: insertions,
// deletions, substitutions, and adjacent transpositions each cost one.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	rows, cols := len(ra)+1, len(rb)+1
	previous2 := make([]int, cols)
	previous := make([]int, cols)

	current := make([]int, cols)
	for j := range cols {
		previous[j] = j
	}

	for i := 1; i < rows; i++ {
		current[0] = i

		for j := 1; j < cols; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}

			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				current[j] = min(current[j], previous2[j-2]+1)
			}
		}

		previous2, previous, current = previous, current, previous2
	}

	return previous[cols-1]
}
