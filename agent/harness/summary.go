package harness

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// CleanCompactionSummary removes leading explicit scratchpad blocks and outer
// summary wrappers, then normalizes blank lines. Tags mentioned within the body
// remain literal historical content. An unterminated leading scratchpad is not
// a summary; an unterminated summary wrapper may still contain useful text.
func CleanCompactionSummary(raw string) string {
	text := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	for {
		before := text
		text = unwrapSummaryFence(text)
		text = stripLeadingScratchpad(text)

		lower := strings.ToLower(text)
		if strings.HasPrefix(lower, "<summary>") {
			text = strings.TrimSpace(text[len("<summary>"):])
			if strings.HasSuffix(strings.ToLower(text), "</summary>") {
				text = strings.TrimSpace(text[:len(text)-len("</summary>")])
			}
		}

		if text == before {
			break
		}
	}

	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	body := false

	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			if len(out) == 0 || out[len(out)-1] == "" {
				continue
			}

			line = ""
		} else if !summaryWrapperLine(line) {
			body = true
		}

		out = append(out, line)
	}

	if !body {
		return ""
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ValidateCompactionSummary returns cleaned substantive text or an error
// matching [ErrInvalidCompactionSummary]. minChars counts Unicode characters,
// not bytes, and must be positive. It is a quality heuristic, not a guarantee
// of fidelity. Finish reason is intentionally absent: useful length-truncated
// output is not rejected solely because it was truncated.
func ValidateCompactionSummary(raw string, minChars int) (string, error) {
	if minChars <= 0 {
		return "", fmt.Errorf("%w: minimum characters must be positive", ErrInvalidCompactionSummary)
	}

	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrInvalidCompactionSummary)
	}

	cleaned := CleanCompactionSummary(raw)
	chars := 0

	for line := range strings.SplitSeq(cleaned, "\n") {
		if !summaryWrapperLine(line) {
			chars += utf8.RuneCountInString(strings.TrimSpace(line))
		}
	}

	if chars < minChars {
		return "", fmt.Errorf("%w: cleaned body has %d characters; need at least %d", ErrInvalidCompactionSummary, chars, minChars)
	}

	return cleaned, nil
}

func stripLeadingScratchpad(text string) string {
	for _, tag := range []string{"analysis", "think", "thinking", "scratchpad", "reasoning"} {
		open, closeTag := "<"+tag+">", "</"+tag+">"
		if len(text) < len(open) || !strings.EqualFold(text[:len(open)], open) {
			continue
		}

		end := indexSummaryTag(text[len(open):], closeTag)
		if end < 0 {
			return ""
		}

		return strings.TrimSpace(text[len(open)+end+len(closeTag):])
	}

	return text
}

func indexSummaryTag(text, tag string) int {
	for offset := 0; offset < len(text); {
		next := strings.IndexByte(text[offset:], '<')
		if next < 0 {
			return -1
		}

		position := offset + next
		if len(text)-position >= len(tag) && strings.EqualFold(text[position:position+len(tag)], tag) {
			return position
		}

		offset = position + 1
	}

	return -1
}

func unwrapSummaryFence(text string) string {
	first, rest, found := strings.Cut(text, "\n")
	if !found || !strings.HasSuffix(rest, "\n```") {
		return text
	}

	switch strings.ToLower(strings.TrimSpace(first)) {
	case "```", "```xml", "```markdown", "```md", "```text", "```summary":
		return strings.TrimSpace(strings.TrimSuffix(rest, "\n```"))
	default:
		return text
	}
}

func summaryWrapperLine(line string) bool {
	line = strings.ToLower(strings.Trim(strings.TrimSpace(line), "-*_`# \t"))
	switch line {
	case "", "summary", "summary:", "<summary>", "</summary>", "turn context (split turn):":
		return true
	default:
		return false
	}
}
