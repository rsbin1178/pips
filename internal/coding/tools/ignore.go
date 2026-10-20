//nolint:wsl_v5 // Pattern parsing and precedence checks stay adjacent.
package tools

import (
	"bufio"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// ignoreDecision is the outcome of matching one path against the ignore stack.
type ignoreDecision uint8

const (
	ignoreNone ignoreDecision = iota
	ignoreMatched
	ignoreWhitelisted
)

const (
	ignoreFileBytes = 1 << 20
	ignoreFileName  = ".gitignore"
	plainIgnoreName = ".ignore"
	gitDirName      = ".git"
)

// ignoreRule is one parsed gitignore pattern. Patterns are matched relative to
// base, the workspace-relative directory that owns the file.
type ignoreRule struct {
	base     string
	pattern  string
	negated  bool
	dirOnly  bool
	anchored bool
}

// ignoreMatcher implements the subset of the `ignore` crate contract that
// list_dir and grep expose: .ignore, .gitignore, .git/info/exclude and the
// configured global gitignore. Read errors fail open, as the reference walker
// treats an unreadable ignore file as no rule.
type ignoreMatcher struct {
	tree      *workspace.Tree
	gitRoot   string
	global    []ignoreRule
	exclude   []ignoreRule
	rules     map[string][]ignoreRule
	patterns  map[string][]ignoreRule
	gitActive bool
}

func newIgnoreMatcher(tree *workspace.Tree) *ignoreMatcher {
	matcher := &ignoreMatcher{
		tree:     tree,
		rules:    make(map[string][]ignoreRule),
		patterns: make(map[string][]ignoreRule),
	}
	matcher.gitRoot = findGitRoot(tree)
	matcher.gitActive = matcher.gitRoot != ""
	if matcher.gitActive {
		matcher.exclude = matcher.loadRules(gitDirName+"/info/exclude", ".")
	}
	matcher.global = matcher.loadGlobalRules()
	return matcher
}

// ignored reports whether rel (a workspace-relative slash path) is excluded.
// Hidden entries stay hidden unless an ignore whitelist explicitly re-includes
// them, matching the reference walker's hidden filter.
func (m *ignoreMatcher) ignored(rel string, isDir bool) bool {
	if rel == "" || rel == "." {
		return false
	}

	decision := m.match(rel, isDir)
	if decision == ignoreWhitelisted {
		return false
	}
	if decision == ignoreMatched {
		return true
	}

	return hiddenPath(rel)
}

func (m *ignoreMatcher) match(rel string, isDir bool) ignoreDecision {
	decision := ignoreNone

	apply := func(rules []ignoreRule) {
		for _, rule := range rules {
			matched := rule.match(rel, isDir)
			if !matched {
				continue
			}
			if rule.negated {
				decision = ignoreWhitelisted
			} else {
				decision = ignoreMatched
			}
		}
	}

	if m.gitActive {
		apply(m.global)
		apply(m.exclude)
	}

	dirs := pathChain(rel, isDir)
	for _, dir := range dirs {
		apply(m.rulesFor(dir, ignoreFileName))
	}
	for _, dir := range dirs {
		apply(m.rulesFor(dir, plainIgnoreName))
	}

	return decision
}

func (r ignoreRule) match(rel string, isDir bool) bool {
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return false
	}

	base := r.base
	if base == "" {
		base = "."
	}

	if base != "." {
		if rel == base {
			return false
		}
		if !strings.HasPrefix(rel, base+"/") {
			return false
		}
		rel = strings.TrimPrefix(rel, base+"/")
	}

	pattern := strings.TrimPrefix(r.pattern, "/")
	if pattern == "" {
		return false
	}

	if isDir {
		return r.matchOne(rel, pattern)
	}

	// A matched ancestor directory ignores every descendant.
	for _, ancestor := range ancestorPaths(rel) {
		if r.matchOne(ancestor, pattern) {
			return true
		}
	}

	if r.dirOnly {
		return false
	}

	return r.matchOne(rel, pattern)
}

func (r ignoreRule) matchOne(rel, pattern string) bool {
	if strings.Contains(pattern, "/") || r.anchored {
		matched, err := doublestar.Match(pattern, rel)
		return err == nil && matched
	}

	// An unanchored pattern with no slash matches a basename at any depth.
	matched, err := doublestar.Match(pattern, path.Base(rel))
	return err == nil && matched
}

func pathChain(rel string, isDir bool) []string {
	clean := strings.Trim(rel, "/")
	if clean == "" || clean == "." {
		return []string{"."}
	}

	parts := strings.Split(clean, "/")
	if !isDir {
		parts = parts[:len(parts)-1]
	}

	dirs := make([]string, 0, len(parts)+1)
	dirs = append(dirs, ".")
	for index := range parts {
		dirs = append(dirs, strings.Join(parts[:index+1], "/"))
	}

	return dirs
}

func ancestorPaths(rel string) []string {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) <= 1 {
		return nil
	}

	result := make([]string, 0, len(parts)-1)
	for index := 1; index < len(parts); index++ {
		result = append(result, strings.Join(parts[:index], "/"))
	}

	return result
}

func hiddenPath(rel string) bool {
	for segment := range strings.SplitSeq(strings.Trim(rel, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			continue
		}
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}

	return false
}

func (m *ignoreMatcher) rulesFor(dir, name string) []ignoreRule {
	key := dir + "\x00" + name
	if rules, ok := m.patterns[key]; ok {
		return rules
	}

	rel := name
	if dir != "." {
		rel = dir + "/" + name
	}
	rules := m.loadRules(rel, dir)
	m.patterns[key] = rules

	return rules
}

func (m *ignoreMatcher) loadRules(rel, base string) []ignoreRule {
	if rules, ok := m.rules[rel]; ok {
		return rules
	}

	file, err := m.tree.Open(rel)
	if err != nil {
		m.rules[rel] = nil
		return nil
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, ignoreFileBytes+1))
	if err != nil || len(data) > ignoreFileBytes {
		m.rules[rel] = nil
		return nil
	}

	rules := parseIgnoreRules(string(data), base)
	m.rules[rel] = rules

	return rules
}

func parseIgnoreRules(content, base string) []ignoreRule {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64<<10), ignoreFileBytes)

	rules := make([]ignoreRule, 0)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		rule, ok := parseIgnoreRule(line, base)
		if ok {
			rules = append(rules, rule)
		}
	}

	return rules
}

// parseIgnoreRule implements gitignore pattern parsing for the forms the
// matcher supports: comments, escaped leading !/#, negation, anchoring,
// directory-only patterns, trailing-space trimming, and ** globs.
func parseIgnoreRule(line, base string) (ignoreRule, bool) {
	line = strings.TrimSuffix(line, "\r")
	if line == "" {
		return ignoreRule{}, false
	}
	if strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}

	line = trimIgnoreTrailingSpaces(line)
	if line == "" {
		return ignoreRule{}, false
	}

	rule := ignoreRule{base: base}
	if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
		line = line[1:]
	} else if strings.HasPrefix(line, "!") {
		rule.negated = true
		line = strings.TrimPrefix(line, "!")
	}

	if strings.HasSuffix(line, "/") {
		rule.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if strings.HasPrefix(line, "/") {
		rule.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	if strings.Contains(line, "/") {
		rule.anchored = true
	}
	if line == "" {
		return ignoreRule{}, false
	}

	rule.pattern = line

	return rule, true
}

func trimIgnoreTrailingSpaces(value string) string {
	end := len(value)
	for end > 0 && value[end-1] == ' ' {
		backslashes := 0
		for index := end - 2; index >= 0 && value[index] == '\\'; index-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		end--
	}

	return value[:end]
}

func findGitRoot(tree *workspace.Tree) string {
	for current := "."; ; {
		info, err := tree.Lstat(gitMarker(current))
		if err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return current
		}

		if current == "." {
			return ""
		}

		parent := path.Dir(current)
		if parent == "." || parent == "/" {
			current = "."
		} else {
			current = parent
		}
	}
}

func gitMarker(dir string) string {
	if dir == "." {
		return gitDirName
	}

	return dir + "/" + gitDirName
}

// globalIgnorePath resolves git's core.excludesFile, falling back to the XDG
// default. It reads configuration outside the workspace, which is read-only
// process configuration rather than a workspace file operation.
func globalIgnorePath() string {
	configs := make([]string, 0, 3)
	if value := strings.TrimSpace(os.Getenv("GIT_CONFIG_GLOBAL")); value != "" {
		configs = append(configs, expandHome(value))
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		configs = append(configs, filepath.Join(home, ".gitconfig"))
	}
	if value := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); value != "" {
		configs = append(configs, filepath.Join(expandHome(value), "git", "config"))
	} else if home != "" {
		configs = append(configs, filepath.Join(home, ".config", "git", "config"))
	}

	for _, config := range configs {
		if excludes := parseCoreExcludes(config); excludes != "" {
			return excludes
		}
	}

	if value := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); value != "" {
		return filepath.Join(expandHome(value), "git", "ignore")
	}
	if home != "" {
		return filepath.Join(home, ".config", "git", "ignore")
	}

	return ""
}

func parseCoreExcludes(name string) string {
	file, err := os.Open(name) //nolint:gosec // Reads git's own configuration file.
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()

	inCore := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.ToLower(strings.Trim(line, "[]"))
			inCore = section == "core"
			continue
		}
		if !inCore {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "excludesfile") {
			continue
		}

		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"`)
		if value == "" {
			continue
		}

		return expandHome(value)
	}

	return ""
}

func expandHome(value string) string {
	if value == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return value
		}

		return home
	}
	if strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return value
		}

		return filepath.Join(home, value[2:])
	}

	return value
}

func (m *ignoreMatcher) loadGlobalRules() []ignoreRule {
	name := globalIgnorePath()
	if name == "" {
		return nil
	}

	file, err := os.Open(name) //nolint:gosec // Reads git's configured global ignore file.
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, ignoreFileBytes+1))
	if err != nil || len(data) > ignoreFileBytes {
		return nil
	}

	return parseIgnoreRules(string(data), ".")
}
