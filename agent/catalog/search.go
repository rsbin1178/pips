package catalog

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/rsbin1178/pips/ai"
)

// Field weights for one query term hit. Name and parameter-name hits outrank
// free-text hits so `web_search` ranks above a tool whose description merely
// mentions searching.
const (
	searchNameWeight    = 3
	searchParamWeight   = 2
	searchTextWeight    = 1
	searchTextTermCap   = 3
	searchAllTermsBonus = 1
	searchPrefixMinimum = 3
)

// SearchTokens returns the lowercase tokens that Search matches a query term
// against. Values split on non-alphanumerics and camelCase boundaries, and one
// trailing plural "s" is removed from tokens of four or more characters so
// "tools" and "tool" compare equal. The same tokenizer is exposed so callers
// can rank tool names consistently with Search.
func SearchTokens(value string) []string {
	var (
		tokens  []string
		current strings.Builder
		runes   = []rune(value)
	)

	flush := func() {
		if current.Len() == 0 {
			return
		}

		tokens = append(tokens, stemToken(strings.ToLower(current.String())))
		current.Reset()
	}

	for index, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}

		if unicode.IsUpper(r) && index > 0 && current.Len() > 0 {
			previous := runes[index-1]

			nextLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && nextLower) {
				flush()
			}
		}

		current.WriteRune(r)
	}

	flush()

	return tokens
}

func stemToken(token string) string {
	if len(token) >= 4 && strings.HasSuffix(token, "s") && !strings.HasSuffix(token, "ss") {
		return token[:len(token)-1]
	}

	return token
}

// searchDocument is the per-entry index built once at catalog construction.
type searchDocument struct {
	name   map[string]struct{}
	params map[string]struct{}
	text   map[string]int
}

func buildSearchDocument(entry Entry) searchDocument {
	decl := entry.Tool.Decl()

	doc := searchDocument{
		name:   make(map[string]struct{}),
		params: make(map[string]struct{}),
		text:   make(map[string]int),
	}
	for _, token := range SearchTokens(decl.Name) {
		doc.name[token] = struct{}{}
	}

	doc.addText(decl.Description)
	doc.addText(entry.Source.Kind)
	doc.addText(entry.Source.ID)

	for _, tag := range entry.Tags {
		doc.addText(tag)
	}

	for _, parameter := range schemaParameters(decl.InputSchema) {
		for _, token := range SearchTokens(parameter.name) {
			doc.params[token] = struct{}{}
		}

		doc.addText(parameter.description)
	}

	return doc
}

func (d *searchDocument) addText(value string) {
	for _, token := range SearchTokens(value) {
		d.text[token]++
	}
}

// score sums the weighted hits for every query term. A term hits a field when
// it equals a token or, at three or more characters, prefixes one; the bonus
// applies only when every term hit at least one field.
func (d searchDocument) score(terms []string) int {
	total := 0

	matchedAll := len(terms) > 0
	for _, term := range terms {
		termScore := 0
		if tokenSetMatches(d.name, term) {
			termScore += searchNameWeight
		}

		if tokenSetMatches(d.params, term) {
			termScore += searchParamWeight
		}

		termScore += min(d.textFrequency(term), searchTextTermCap) * searchTextWeight
		if termScore == 0 {
			matchedAll = false
		}

		total += termScore
	}

	if matchedAll {
		total += searchAllTermsBonus
	}

	return total
}

func (d searchDocument) textFrequency(term string) int {
	if count, ok := d.text[term]; ok {
		return count
	}

	if len(term) < searchPrefixMinimum {
		return 0
	}

	frequency := 0

	for token, count := range d.text {
		if strings.HasPrefix(token, term) {
			frequency += count
		}
	}

	return frequency
}

func tokenSetMatches(set map[string]struct{}, term string) bool {
	if _, ok := set[term]; ok {
		return true
	}

	if len(term) < searchPrefixMinimum {
		return false
	}

	for token := range set {
		if strings.HasPrefix(token, term) {
			return true
		}
	}

	return false
}

type schemaParameter struct {
	name        string
	description string
}

// schemaParameters lists the top-level object properties of a tool input
// schema. Typed schemas expose Properties directly; MCP schemas arrive as raw
// JSON and are decoded leniently because a malformed remote schema must not
// make the tool undiscoverable.
func schemaParameters(schema *ai.Schema) []schemaParameter {
	if schema == nil {
		return nil
	}

	var parameters []schemaParameter

	if len(schema.RawJSON) > 0 {
		var raw struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(schema.RawJSON, &raw); err != nil {
			return nil
		}

		for name, property := range raw.Properties {
			var detail struct {
				Description string `json:"description"`
			}
			// Boolean property schemas carry no description; ignore decode errors.
			_ = json.Unmarshal(property, &detail)
			parameters = append(parameters, schemaParameter{name: name, description: detail.Description})
		}
	} else {
		for name, property := range schema.Properties {
			parameter := schemaParameter{name: name}
			if property != nil {
				parameter.description = property.Description
			}

			parameters = append(parameters, parameter)
		}
	}

	sort.Slice(parameters, func(i, j int) bool { return parameters[i].name < parameters[j].name })

	return parameters
}
