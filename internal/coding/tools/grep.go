//nolint:wsl_v5 // The bounded search pass keeps each mode branch adjacent.
package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const grepName = "grep"

const (
	grepContentMode = "content"
	grepFilesMode   = "files_with_matches"
	grepCountMode   = "count"
	grepMaxContext  = 1_000
	grepWSLTimeout  = 60 * time.Second
	// grepCountMatchCeiling bounds per-file match storage in count mode for a
	// pathological zero-width pattern; ordinary files never reach it.
	grepCountMatchCeiling = 200_000
)

type grepArgs struct {
	Pattern       string  `json:"pattern" description:"Go RE2 regular expression, or literal text when fixed_strings is true"`
	FixedStrings  *bool   `json:"fixed_strings" description:"Treat pattern as literal text instead of a regular expression"`
	Path          *string `json:"path" description:"Optional workspace file or directory (relative, or absolute inside the workspace)"`
	Glob          *string `json:"glob" description:"Optional slash-separated file glob; a leading ! excludes matches"`
	CaseSensitive *bool   `json:"case_sensitive" description:"Whether matching is case-sensitive"`
	BeforeContext *int    `json:"before_context" description:"Number of lines to show before each match"`
	AfterContext  *int    `json:"after_context" description:"Number of lines to show after each match"`
	Context       *int    `json:"context" description:"Number of lines to show before and after each match"`
	Type          *string `json:"type" description:"File type filter such as go, js, py, rust, or a bare extension"`
	Multiline     *bool   `json:"multiline" description:"Enable multiline mode where . matches newlines and patterns can span lines"`
	OutputMode    *string `json:"output_mode" description:"content (default), files_with_matches, or count"`
	Offset        *int    `json:"offset" description:"Optional zero-based matching-entry offset"`
	Limit         *int    `json:"limit" description:"Optional maximum number of matching entries"`
}

type grepMatch struct {
	startLine int
	endLine   int
}

type grepFileResult struct {
	path           string
	matches        int
	body           string
	matchSet       map[int]bool
	truncatedLines int
}

//nolint:gocyclo,funlen // Search policy, mode formatting, and bounded traversal share one ordered pass.
func (s *service) grep(ctx context.Context, args grepArgs) (string, error) {
	expression, err := compileSearch(args)
	if err != nil {
		return "", failure(grepName, err)
	}

	base := "."
	if args.Path != nil && strings.TrimSpace(*args.Path) != "" {
		base = *args.Path
	}

	base, info, err := inspectSearchPath(s.tree, base)
	if err != nil {
		return "", failure(grepName, err)
	}
	searchFile := info.Mode().IsRegular()

	glob := ""
	if args.Glob != nil && strings.TrimSpace(*args.Glob) != "" {
		glob = strings.TrimSpace(*args.Glob)
		validated := strings.TrimPrefix(glob, "!")
		if _, err := validatePattern(validated); err != nil {
			return "", failure(grepName, err)
		}
	}

	typeGlobs, err := grepTypeGlobs(args.Type)
	if err != nil {
		return "", failure(grepName, err)
	}

	before, after, err := grepContext(args)
	if err != nil {
		return "", failure(grepName, err)
	}

	mode, err := grepOutputMode(args.OutputMode)
	if err != nil {
		return "", failure(grepName, err)
	}

	offset, limit, err := grepWindow(args.Offset, args.Limit, mode, s.limits)
	if err != nil {
		return "", failure(grepName, err)
	}

	parentCtx := ctx
	timeout := grepTimeout(s.limits)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	matcher := newIgnoreMatcher(s.tree)

	value := result{OK: true, Tool: grepName}
	body := make([]string, 0)
	bodyBytes := 0
	emitted := 0
	matched := 0
	totalMatches := 0
	totalFiles := 0
	truncated := false
	reason := ""
	nextOffset := offset
	lastProgress := time.Now()

	stop := func(stopReason string, next int) error {
		truncated = true
		reason = stopReason
		nextOffset = next

		return errWalkComplete
	}

	consider := func(rel string, explicit bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		value.Counts.Scanned++
		if value.Counts.Scanned > s.limits.ScanEntries {
			return stop("scan_entries", offset+emitted)
		}

		if value.Counts.Files >= s.limits.GrepFiles {
			return stop("files_scanned", offset+emitted)
		}

		matchRel := rel
		if base != "." && strings.HasPrefix(rel, base+"/") {
			matchRel = strings.TrimPrefix(rel, base+"/")
		}
		if !grepPathIncluded(matchRel, glob, typeGlobs) {
			return nil
		}

		data, readErr := readRegularFile(ctx, s.tree, rel, s.limits.GrepFileBytes)
		if errors.Is(readErr, errFileTooLarge) || errors.Is(readErr, errBinaryFile) {
			value.Counts.Skipped++

			return nil
		}
		if readErr != nil {
			if explicit {
				return readErr
			}
			value.Counts.Skipped++

			return nil
		}

		value.Counts.Files++
		matchLimit := offset + limit + 1
		switch mode {
		case grepFilesMode:
			matchLimit = 1
		case grepCountMode:
			matchLimit = grepCountMatchCeiling
		}
		fileResult, occurrences, _ := s.grepFile(
			expression, rel, data, args, before, after, matchLimit,
		)
		if occurrences == 0 {
			return nil
		}

		//nolint:nestif // Content and file/count pagination need different skip units.
		if mode == grepContentMode {
			if matched < offset {
				skip := min(offset-matched, occurrences)
				matched += skip
				if skip == occurrences {
					return nil
				}
				fileResult = trimGrepFileResult(fileResult, skip)
				occurrences -= skip
			}

			if emitted >= limit {
				return stop("matches", offset+emitted)
			}

			remainingEntries := limit - emitted
			if occurrences > remainingEntries {
				fileResult = trimGrepFileResult(fileResult, remainingEntries)
				occurrences = remainingEntries
				truncated = true
				reason = "matches"
				nextOffset = offset + emitted + occurrences
			}

			if len(body) > 0 {
				if !appendGrepChunk(&body, &bodyBytes, "\n", s.limits.GrepOutputBytes) {
					return stop(reasonBytes, offset+emitted)
				}
			}
			accepted, complete := appendGrepBounded(
				&body, &bodyBytes, fileResult.body, s.limits.GrepOutputBytes,
			)
			renderedMatches := grepBodyMatchLines(accepted)
			if renderedMatches > 0 {
				totalFiles++
			}
			totalMatches += renderedMatches
			value.Counts.Lines += renderedMatches
			value.Counts.TruncatedLines += fileResult.truncatedLines

			if !complete {
				emitted += renderedMatches
				matched += renderedMatches
				nextOffset = offset + emitted

				return stop(reasonBytes, nextOffset)
			}

			emitted += occurrences
			matched += occurrences
			if emitted >= limit {
				truncated = true
				reason = "matches"
				nextOffset = offset + emitted
			}
		} else {
			if matched < offset {
				matched++

				return nil
			}
			if emitted >= limit {
				return stop("files", offset+emitted)
			}

			rendered := fileResult.path + "\n"
			if mode == grepCountMode {
				totalMatches += fileResult.matchLines()
				rendered = fileResult.path + ":" + strconv.Itoa(fileResult.matchLines()) + "\n"
			}
			if !appendGrepChunk(&body, &bodyBytes, rendered, s.limits.GrepOutputBytes) {
				return stop(reasonBytes, offset+emitted)
			}

			emitted++
			matched++
			totalFiles++
			if emitted >= limit {
				truncated = true
				reason = "files"
				nextOffset = offset + emitted
			}
		}

		if time.Since(lastProgress) >= 250*time.Millisecond {
			reportToolProgress(ctx, grepName, value.Counts)

			lastProgress = time.Now()
		}

		if truncated {
			return errWalkComplete
		}

		return nil
	}

	var walkErr error
	if searchFile {
		// An explicit file path is searched even when a traversal would skip it,
		// matching ripgrep's explicit-path behavior.
		walkErr = consider(base, true)
	} else {
		walkErr = s.walkGrep(ctx, base, matcher, consider)
	}

	if walkErr != nil && !errors.Is(walkErr, errWalkComplete) {
		if parentCtx.Err() != nil {
			return "", failure(grepName, parentCtx.Err())
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return s.grepTimeoutResult(
				totalMatches, totalFiles, emitted, offset, timeout, body, mode,
			)
		}

		return "", failure(grepName, fmt.Errorf("coding tools: grep: %w", walkErr))
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return s.grepTimeoutResult(
			totalMatches, totalFiles, emitted, offset, timeout, body, mode,
		)
	}

	summary := ""
	switch mode {
	case grepContentMode:
		summary = fmt.Sprintf("Found %s%d matching lines", atLeast(truncated), totalMatches)
	case grepFilesMode:
		summary = fmt.Sprintf("Found %s%d files", atLeast(truncated), totalFiles)
	case grepCountMode:
		summary = fmt.Sprintf("Found %d across %s%d files", totalMatches, atLeast(truncated), totalFiles)
	}

	if emitted == 0 {
		summary = "No matches found"
	}

	var out strings.Builder
	out.WriteString(summary)
	out.WriteByte('\n')
	for _, chunk := range body {
		out.WriteString(chunk)
	}
	if truncated {
		out.WriteString(grepTruncationFooter(mode, emitted, offset+emitted))
	}

	value.Body = out.String()
	value.Truncated = truncated
	value.Reason = reason
	value.Counts.Bytes = out.Len()
	value.Counts.Files = totalFiles
	value.Counts.Matches = totalMatches
	if truncated {
		value.Next.Offset = &nextOffset
	}

	return value.render(), nil
}

func (s *service) grepTimeoutResult(
	totalMatches, totalFiles, emitted, offset int,
	timeout time.Duration,
	chunks []string,
	mode string,
) (string, error) {
	var out strings.Builder
	if emitted > 0 {
		switch mode {
		case grepContentMode:
			fmt.Fprintf(&out, "Found at least %d matching lines\n", totalMatches)
		case grepFilesMode:
			fmt.Fprintf(&out, "Found at least %d files\n", totalFiles)
		case grepCountMode:
			fmt.Fprintf(&out, "Found %d across at least %d files\n", totalMatches, totalFiles)
		}
		for _, chunk := range chunks {
			out.WriteString(chunk)
		}
		fmt.Fprintf(
			&out,
			"... [grep timed out after %d seconds; the matches above are partial] ...\n",
			int(timeout.Seconds()),
		)
	} else {
		fmt.Fprintf(
			&out,
			"grep search timed out after %d seconds. The search may have matched files but did not complete in time. Try searching a more specific path or pattern.\n",
			int(timeout.Seconds()),
		)
	}

	value := result{
		OK: true, Tool: grepName, Body: out.String(), Truncated: true, Reason: "timeout",
		Counts: ResultCounts{Matches: totalMatches, Files: totalFiles},
	}
	next := offset + emitted
	value.Next.Offset = &next

	return value.render(), nil
}

func grepTruncationFooter(mode string, emitted, next int) string {
	entry := "matching line"
	if mode != grepContentMode {
		entry = "file"
	}

	return fmt.Sprintf(
		"... [output truncated after %d %ss; continue with offset %d] ...\n",
		emitted,
		entry,
		next,
	)
}

func atLeast(truncated bool) string {
	if truncated {
		return "at least "
	}

	return ""
}

func appendGrepBounded(body *[]string, size *int, chunk string, maxBytes int) (string, bool) {
	if *size+len(chunk) <= maxBytes {
		*body = append(*body, chunk)
		*size += len(chunk)

		return chunk, true
	}

	remaining := maxBytes - *size
	if remaining <= 0 {
		return "", false
	}

	truncated, _ := truncateUTF8(chunk, remaining)
	if truncated == "" {
		return "", false
	}
	*body = append(*body, truncated)
	*size += len(truncated)

	return truncated, false
}

func appendGrepChunk(body *[]string, size *int, chunk string, maxBytes int) bool {
	if *size+len(chunk) > maxBytes {
		return false
	}
	*body = append(*body, chunk)
	*size += len(chunk)

	return true
}

func (s *service) walkGrep(
	ctx context.Context,
	base string,
	matcher *ignoreMatcher,
	visit func(string, bool) error,
) error {
	return fs.WalkDir(s.tree.FileSystem(), base, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if name == base {
				return walkErr
			}

			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == base {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}

			return nil
		}
		if entry.IsDir() {
			if entry.Name() == gitDirName || matcher.ignored(name, true) {
				return fs.SkipDir
			}

			return nil
		}
		if matcher.ignored(name, false) {
			return nil
		}

		return visit(name, false)
	})
}

func grepPathIncluded(rel, glob string, typeGlobs []string) bool {
	if len(typeGlobs) > 0 && !matchTypeGlobs(typeGlobs, rel) {
		return false
	}
	if glob == "" {
		return true
	}
	if excluded, ok := strings.CutPrefix(glob, "!"); ok {
		return !matchGrepGlob(excluded, rel)
	}

	return matchGrepGlob(glob, rel)
}

func matchGrepGlob(pattern, rel string) bool {
	pattern = strings.TrimPrefix(pattern, "/")
	if strings.Contains(pattern, "/") {
		matched, err := doublestar.Match(pattern, rel)
		return err == nil && matched
	}

	matched, err := doublestar.Match(pattern, path.Base(rel))
	return err == nil && matched
}

func matchTypeGlobs(patterns []string, rel string) bool {
	base := path.Base(rel)
	for _, pattern := range patterns {
		matched, err := doublestar.Match(pattern, base)
		if err == nil && matched {
			return true
		}
		matched, err = doublestar.Match(pattern, rel)
		if err == nil && matched {
			return true
		}
	}

	return false
}

func grepTypeGlobs(value *string) ([]string, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, nil
	}

	name := strings.ToLower(strings.TrimSpace(*value))
	known := map[string][]string{
		"c":          {"*.c", "*.h"},
		"cpp":        {"*.cc", "*.cpp", "*.cxx", "*.hpp", "*.hxx", "*.hh"},
		"c++":        {"*.cc", "*.cpp", "*.cxx", "*.hpp", "*.hxx", "*.hh"},
		"cs":         {"*.cs"},
		"csharp":     {"*.cs"},
		"css":        {"*.css"},
		"dart":       {"*.dart"},
		"docker":     {"Dockerfile", "*.dockerfile"},
		"elixir":     {"*.ex", "*.exs"},
		"go":         {"*.go"},
		"graphql":    {"*.graphql", "*.gql"},
		"hcl":        {"*.hcl"},
		"ini":        {"*.ini"},
		"lua":        {"*.lua"},
		"make":       {"Makefile", "*.mk"},
		"perl":       {"*.pl", "*.pm"},
		"r":          {"*.r", "*.R"},
		"svelte":     {"*.svelte"},
		"terraform":  {"*.tf", "*.tfvars"},
		"txt":        {"*.txt"},
		"vue":        {"*.vue"},
		"golang":     {"*.go"},
		"html":       {"*.html", "*.htm"},
		"java":       {"*.java"},
		"js":         {"*.js", "*.jsx", "*.mjs", "*.cjs"},
		"javascript": {"*.js", "*.jsx", "*.mjs", "*.cjs"},
		"json":       {"*.json"},
		"jsx":        {"*.jsx"},
		"kt":         {"*.kt", "*.kts"},
		"kotlin":     {"*.kt", "*.kts"},
		"md":         {"*.md", "*.markdown"},
		"markdown":   {"*.md", "*.markdown"},
		"php":        {"*.php"},
		"proto":      {"*.proto"},
		"py":         {"*.py", "*.pyi"},
		"python":     {"*.py", "*.pyi"},
		"rb":         {"*.rb"},
		"ruby":       {"*.rb"},
		"rs":         {"*.rs"},
		"rust":       {"*.rs"},
		"scala":      {"*.scala", "*.sc"},
		"scss":       {"*.scss"},
		"sh":         {"*.sh", "*.bash", "*.zsh"},
		"shell":      {"*.sh", "*.bash", "*.zsh"},
		"sql":        {"*.sql"},
		"swift":      {"*.swift"},
		"toml":       {"*.toml"},
		"ts":         {"*.ts", "*.tsx", "*.mts", "*.cts"},
		"tsx":        {"*.tsx"},
		"typescript": {"*.ts", "*.tsx", "*.mts", "*.cts"},
		"xml":        {"*.xml"},
		"yaml":       {"*.yaml", "*.yml"},
		"yml":        {"*.yaml", "*.yml"},
	}
	if patterns, ok := known[name]; ok {
		return patterns, nil
	}

	return nil, fmt.Errorf("%w: unrecognized file type %q", errInvalidArgument, *value)
}

func grepContext(args grepArgs) (int, int, error) {
	before, after := 0, 0
	if args.Context != nil {
		before = *args.Context
		after = *args.Context
	}
	if args.BeforeContext != nil {
		before = *args.BeforeContext
	}
	if args.AfterContext != nil {
		after = *args.AfterContext
	}
	if before < 0 || after < 0 || before > grepMaxContext || after > grepMaxContext {
		return 0, 0, fmt.Errorf(
			"%w: context must be between 0 and %d", errInvalidArgument, grepMaxContext,
		)
	}

	return before, after, nil
}

func grepOutputMode(value *string) (string, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return grepContentMode, nil
	}
	switch strings.ToLower(strings.TrimSpace(*value)) {
	case grepContentMode:
		return grepContentMode, nil
	case grepFilesMode:
		return grepFilesMode, nil
	case grepCountMode:
		return grepCountMode, nil
	default:
		return "", fmt.Errorf(
			"%w: output_mode must be content, files_with_matches, or count", errInvalidArgument,
		)
	}
}

func grepWindow(offsetValue, limitValue *int, mode string, limits Limits) (int, int, error) {
	defaultLimit, maxLimit := limits.GrepMatches, limits.GrepMaxMatches
	if mode != grepContentMode {
		defaultLimit, maxLimit = limits.GrepFileMatches, limits.GrepMaxFileEntries
	}

	offset := 0
	if offsetValue != nil {
		offset = *offsetValue
	}
	limit := defaultLimit
	if limitValue != nil {
		limit = *limitValue
	}
	if offset < 0 || limit < 1 || limit > maxLimit {
		return 0, 0, fmt.Errorf(
			"%w: offset must be non-negative and limit must be between 1 and %d",
			errInvalidArgument,
			maxLimit,
		)
	}

	return offset, limit, nil
}

func grepTimeout(limits Limits) time.Duration {
	if isWSL() && limits.GrepTimeout == DefaultLimits().GrepTimeout {
		return grepWSLTimeout
	}

	return limits.GrepTimeout
}

func isWSL() bool {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(data))

	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

func compileSearch(args grepArgs) (*regexp.Regexp, error) {
	if strings.TrimSpace(args.Pattern) == "" {
		return nil, fmt.Errorf("%w: pattern is required", errInvalidArgument)
	}

	pattern := args.Pattern
	if args.FixedStrings != nil && *args.FixedStrings {
		pattern = regexp.QuoteMeta(pattern)
	}

	caseSensitive := true
	if args.CaseSensitive != nil {
		caseSensitive = *args.CaseSensitive
	}

	flags := ""
	if !caseSensitive {
		flags += "i"
	}
	if args.Multiline != nil && *args.Multiline {
		flags += "ms"
	}
	if flags != "" {
		pattern = "(?" + flags + ":" + pattern + ")"
	}

	expression, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid regular expression: %w", errInvalidArgument, err)
	}

	return expression, nil
}

//nolint:gocyclo // Line and multiline projections intentionally share one bounded renderer.
func (s *service) grepFile(
	expression *regexp.Regexp,
	rel string,
	data []byte,
	args grepArgs,
	before, after, maxMatches int,
) (grepFileResult, int, int) {
	result := grepFileResult{path: rel, matchSet: make(map[int]bool)}

	multiline := args.Multiline != nil && *args.Multiline
	content := string(data)
	if multiline && strings.Contains(content, "\r\n") {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}

	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" && strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return result, 0, 0
	}
	displayLines := make([]string, len(lines))
	for index := range lines {
		displayLines[index] = strings.TrimSuffix(lines[index], "\r")
	}

	var matches []grepMatch
	//nolint:nestif // Line and multiline match projection stay in one bounded builder.
	if multiline {
		lineStarts := make([]int, 0, len(lines))
		offset := 0
		for _, line := range lines {
			lineStarts = append(lineStarts, offset)
			offset += len(line) + 1
		}
		position := 0
		for len(matches) < maxMatches && position <= len(content) {
			location := expression.FindStringIndex(content[position:])
			if location == nil {
				break
			}
			start := position + location[0]
			end := position + location[1]
			startLine := lineIndexAt(lineStarts, start)
			endAt := end
			if endAt > start {
				endAt--
			}
			endLine := lineIndexAt(lineStarts, min(endAt, len(content)))
			matches = append(matches, grepMatch{startLine: startLine, endLine: endLine})
			if end == start {
				position = start + 1
			} else {
				position = end
			}
		}
	} else {
		for index, line := range displayLines {
			if len(matches) >= maxMatches {
				break
			}
			if expression.MatchString(line) {
				matches = append(matches, grepMatch{startLine: index, endLine: index})
			}
		}
	}

	if len(matches) == 0 {
		return result, 0, 0
	}

	include := make(map[int]bool)
	matchLines := 0
	for _, match := range matches {
		for line := match.startLine; line <= match.endLine; line++ {
			include[line] = true
			if !result.matchSet[line] {
				result.matchSet[line] = true
				matchLines++
			}
		}
		for line := max(0, match.startLine-before); line < match.startLine; line++ {
			include[line] = true
		}
		for line := match.endLine + 1; line <= match.endLine+after && line < len(lines); line++ {
			include[line] = true
		}
	}

	keys := make([]int, 0, len(include))
	for line := range include {
		keys = append(keys, line)
	}
	slices.Sort(keys)

	var body strings.Builder
	body.WriteString(rel)
	body.WriteByte('\n')
	for _, line := range keys {
		body.WriteString(strconv.Itoa(line + 1))
		if result.matchSet[line] {
			body.WriteByte(':')
		} else {
			body.WriteByte('-')
		}
		shown, truncated := truncateGrepLine(displayLines[line], s.limits.GrepLineBytes)
		if truncated {
			result.truncatedLines++
		}
		body.WriteString(shown)
		body.WriteByte('\n')
	}
	result.body = body.String()
	result.matches = len(matches)

	return result, len(matches), matchLines
}

func lineIndexAt(starts []int, pos int) int {
	index := sort.Search(len(starts), func(index int) bool { return starts[index] > pos })
	if index == 0 {
		return 0
	}

	return index - 1
}

func trimGrepFileResult(result grepFileResult, keep int) grepFileResult {
	if keep <= 0 {
		result.body = ""
		result.matches = 0

		return result
	}
	if keep >= result.matches {
		return result
	}

	lines := strings.Split(strings.TrimSuffix(result.body, "\n"), "\n")
	header := lines[0]
	seen := 0
	kept := make([]string, 0, len(lines))
	kept = append(kept, header)
	for _, line := range lines[1:] {
		if grepLineIsMatch(line) {
			if seen >= keep {
				break
			}
			seen++
		}
		kept = append(kept, line)
	}
	result.body = strings.Join(kept, "\n") + "\n"
	result.matches = keep

	return result
}

func (f grepFileResult) matchLines() int {
	return len(f.matchSet)
}

func grepLineIsMatch(line string) bool {
	index := 0
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}

	return index > 0 && index < len(line) && line[index] == ':'
}

func grepBodyMatchLines(body string) int {
	total := 0
	for line := range strings.SplitSeq(body, "\n") {
		if grepLineIsMatch(line) {
			total++
		}
	}

	return total
}

func truncateGrepLine(value string, maxChars int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= maxChars {
		return value, false
	}

	return string(runes[:maxChars]) + fmt.Sprintf(" [... truncated (%d chars total)]", len(runes)), true
}

func inspectSearchPath(tree *workspace.Tree, name string) (string, fs.FileInfo, error) {
	normalized, err := tree.NormalizeUserPath(name, true)
	if err != nil {
		return "", nil, err
	}

	if normalized == "." {
		info, statErr := tree.Lstat(".")
		if statErr != nil {
			return "", nil, statErr
		}

		return normalized, info, nil
	}

	parts := strings.Split(normalized, "/")
	for index := range parts {
		current := strings.Join(parts[:index+1], "/")
		info, statErr := tree.Lstat(current)
		if statErr != nil {
			return "", nil, statErr
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("%w: %q", workspace.ErrSymlink, current)
		}
		final := index == len(parts)-1
		if !info.IsDir() {
			if final && info.Mode().IsRegular() {
				return normalized, info, nil
			}

			return "", nil, fmt.Errorf("%w: %q", errNotDirectory, current)
		}
	}

	info, err := tree.Lstat(normalized)
	if err != nil {
		return "", nil, err
	}

	return normalized, info, nil
}
