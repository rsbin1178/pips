//nolint:wsl_v5 // The recursive tree renderer keeps related branches adjacent.
package tools

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/workspace"
)

const lsName = "ls"

const (
	listTopExtensions  = 3
	listRootNoticeText = "Note: this directory is too large to list fully. Try ls on a narrower path, or use grep / shell."
)

type lsArgs struct {
	Path   *string `json:"path" description:"Optional workspace directory path (relative, or absolute inside the workspace); defaults to the workspace root"`
	Offset *int    `json:"offset" description:"Optional zero-based entry offset; providing offset or limit selects flat listing of immediate children"`
	Limit  *int    `json:"limit" description:"Optional maximum number of entries; providing offset or limit selects flat listing of immediate children"`
}

func (s *service) ls(ctx context.Context, args lsArgs) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", failure(lsName, err)
	}

	name := "."
	if args.Path != nil && strings.TrimSpace(*args.Path) != "" {
		name = *args.Path
	}

	normalized, _, err := inspectListPath(s.tree, name)
	if err != nil {
		return "", failure(lsName, err)
	}

	if args.Offset != nil || args.Limit != nil {
		return s.lsFlat(ctx, normalized, args)
	}

	matcher := newIgnoreMatcher(s.tree)
	root, truncated, err := buildListTree(
		ctx, s.tree, matcher, normalized, s.limits.ListTreeItems,
	)
	if err != nil {
		return "", failure(lsName, err)
	}

	body, budgetTruncated := renderListTree(
		root, s.limits.ListOutputChars, s.limits.ListTreeItems, truncated,
	)

	var out strings.Builder
	out.WriteString("- ")
	out.WriteString(normalized)
	out.WriteString("/\n")
	out.WriteString(body)

	value := result{
		OK: true, Tool: lsName, Body: out.String(),
		Truncated: truncated || budgetTruncated,
		Counts: ResultCounts{
			Entries: root.countItems(),
			Files:   root.subtree.totalFiles,
			Bytes:   out.Len(),
		},
	}
	if value.Truncated {
		value.Reason = "entries"
		if budgetTruncated {
			value.Reason = reasonBytes
		}
	}

	return value.render(), nil
}

//nolint:gocyclo // File type decoration and pagination share one bounded loop.
func (s *service) lsFlat(ctx context.Context, normalized string, args lsArgs) (string, error) {
	offset, limit, err := entryWindow(args.Offset, args.Limit, s.limits.ListEntries)
	if err != nil {
		return "", failure(lsName, err)
	}

	entries, err := s.tree.ReadDir(normalized)
	if err != nil {
		return "", failure(lsName, err)
	}

	slices.SortStableFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(asciiLower(a.Name()), asciiLower(b.Name()))
	})

	matcher := newIgnoreMatcher(s.tree)
	visible := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		rel := joinBase(normalized, entry.Name())
		if matcher.ignored(rel, entry.IsDir()) {
			continue
		}
		visible = append(visible, entry)
	}

	var body strings.Builder

	returned := 0
	truncated := false
	reason := ""
	nextOffset := 0

	for index := offset; index < len(visible); index++ {
		if err := ctx.Err(); err != nil {
			return "", failure(lsName, err)
		}

		if returned >= limit {
			truncated = true
			reason = "entries"
			nextOffset = index

			break
		}

		entry := visible[index]

		name := entry.Name()
		switch {
		case entry.IsDir():
			name += "/"
		case entry.Type()&fs.ModeSymlink != 0:
			name += "@"
		case entry.Type() != 0 && !entry.Type().IsRegular():
			name += "?"
		}

		if !appendBounded(&body, name+"\n", s.limits.OutputBytes) {
			truncated = true
			reason = reasonBytes
			nextOffset = index

			break
		}

		returned++
	}

	value := result{
		OK: true, Tool: lsName, Body: body.String(), Truncated: truncated, Reason: reason,
		Counts: ResultCounts{Entries: returned, Bytes: body.Len()},
	}
	if truncated {
		value.Next.Offset = &nextOffset
	}

	return value.render(), nil
}

type listExtensionCounts struct {
	totalFiles int
	byExt      map[string]int
}

func (c *listExtensionCounts) add(ext string) {
	c.totalFiles++
	if c.byExt == nil {
		c.byExt = make(map[string]int)
	}
	c.byExt[ext]++
}

func (c *listExtensionCounts) summary(top int) string {
	if c == nil || c.totalFiles == 0 {
		return ""
	}

	type extensionCount struct {
		ext   string
		count int
	}

	items := make([]extensionCount, 0, len(c.byExt))
	for ext, count := range c.byExt {
		items = append(items, extensionCount{ext: ext, count: count})
	}
	slices.SortFunc(items, func(a, b extensionCount) int {
		if a.count != b.count {
			return b.count - a.count
		}

		return strings.Compare(a.ext, b.ext)
	})

	parts := make([]string, 0, top)
	topSum := 0
	for _, item := range items {
		if len(parts) >= top {
			break
		}
		topSum += item.count
		if item.ext == "no-ext" {
			parts = append(parts, fmt.Sprintf("%d *no-ext", item.count))
		} else {
			parts = append(parts, fmt.Sprintf("%d *.%s", item.count, item.ext))
		}
	}

	ellipsis := ""
	if topSum < c.totalFiles {
		ellipsis = ", ..."
	}
	fileWord := "files"
	if c.totalFiles == 1 {
		fileWord = "file"
	}

	return fmt.Sprintf(
		"[%d %s in subtree: %s%s]",
		c.totalFiles,
		fileWord,
		strings.Join(parts, ", "),
		ellipsis,
	)
}

type listNode struct {
	depth    int
	files    []string
	subdirs  []string
	children map[string]*listNode
	subtree  listExtensionCounts
	expanded bool
}

func newListTreeNode(depth int) *listNode {
	return &listNode{depth: depth, children: make(map[string]*listNode)}
}

func (n *listNode) addItem(parts []string, isDir bool) {
	if len(parts) == 0 {
		return
	}

	name, rest := parts[0], parts[1:]
	if len(rest) == 0 {
		if isDir {
			key := name + "/"
			if _, exists := n.children[key]; !exists {
				n.children[key] = newListTreeNode(n.depth + 1)
				n.subdirs = append(n.subdirs, key)
			}

			return
		}

		n.files = append(n.files, name)
		n.subtree.add(extensionKey(name))

		return
	}

	key := name + "/"
	if _, exists := n.children[key]; !exists {
		n.children[key] = newListTreeNode(n.depth + 1)
		n.subdirs = append(n.subdirs, key)
	}

	child := n.children[key]
	child.addItem(rest, isDir)

	if !isDir {
		n.subtree.add(extensionKey(rest[len(rest)-1]))
	}
}

func (n *listNode) sortRecursive() {
	slices.SortStableFunc(n.files, func(a, b string) int {
		return strings.Compare(asciiLower(a), asciiLower(b))
	})
	slices.SortStableFunc(n.subdirs, func(a, b string) int {
		return strings.Compare(asciiLower(a), asciiLower(b))
	})
	for _, child := range n.children {
		child.sortRecursive()
	}
}

func (n *listNode) allSubitems() []string {
	items := make([]string, 0, len(n.files)+len(n.subdirs))
	items = append(items, n.files...)
	items = append(items, n.subdirs...)
	slices.SortStableFunc(items, func(a, b string) int {
		return strings.Compare(asciiLower(a), asciiLower(b))
	})

	return items
}

func (n *listNode) subitemLine(name string) string {
	return strings.Repeat("  ", n.depth+1) + "- " + name
}

func (n *listNode) summary() string {
	return n.subtree.summary(listTopExtensions)
}

func (n *listNode) countItems() int {
	total := len(n.files) + len(n.subdirs)
	for _, child := range n.children {
		total += child.countItems()
	}

	return total
}

func (n *listNode) summaryCharCost() int {
	summary := n.summary()
	if summary == "" {
		return 0
	}

	return (n.depth+1)*2 + len(summary) + 1
}

func (n *listNode) renderExpanded() string {
	var out strings.Builder
	for _, name := range n.allSubitems() {
		out.WriteString(n.subitemLine(name))
		out.WriteByte('\n')
		if child := n.children[name]; child != nil {
			out.WriteString(child.renderSubtree())
		}
	}

	return out.String()
}

func (n *listNode) renderSubtree() string {
	if n.expanded {
		return n.renderExpanded()
	}

	summary := n.summary()
	if summary == "" {
		return ""
	}

	return strings.Repeat("  ", n.depth+1) + summary + "\n"
}

//nolint:gocyclo // Seed, deep walk, and item caps are one ordered traversal.
func buildListTree(
	ctx context.Context,
	tree *workspace.Tree,
	matcher *ignoreMatcher,
	root string,
	maxItems int,
) (*listNode, bool, error) {
	node := newListTreeNode(0)

	entries, err := tree.ReadDir(root)
	if err != nil {
		return nil, false, err
	}

	seedCount := 0
	truncated := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if entry.Name() == gitDirName {
			continue
		}

		rel := joinBase(root, entry.Name())
		if matcher.ignored(rel, entry.IsDir()) {
			continue
		}

		seedCount++
		if seedCount > maxItems {
			truncated = true

			break
		}

		node.addItem([]string{entry.Name()}, entry.IsDir())
	}

	itemCount := 0
	walkErr := fs.WalkDir(tree.FileSystem(), root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// The reference walker skips an unreadable entry and continues.
			if name == root {
				return walkErr
			}

			return nil
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		relative, depth := listRelative(root, name)
		if depth <= 1 {
			if entry.IsDir() {
				rel := name
				if entry.Name() == gitDirName || matcher.ignored(rel, true) {
					return fs.SkipDir
				}
			}

			return nil
		}

		rel := name
		if entry.IsDir() && (entry.Name() == gitDirName || matcher.ignored(rel, true)) {
			return fs.SkipDir
		}
		if !entry.IsDir() && matcher.ignored(rel, false) {
			return nil
		}

		itemCount++
		if itemCount > maxItems {
			truncated = true

			return fs.SkipAll
		}

		node.addItem(strings.Split(relative, "/"), entry.IsDir())

		return nil
	})
	if walkErr != nil {
		return nil, false, walkErr
	}

	node.sortRecursive()

	return node, truncated, nil
}

func listRelative(root, name string) (string, int) {
	if root == "." {
		if name == "." {
			return "", 0
		}

		return name, strings.Count(name, "/") + 1
	}
	if name == root {
		return "", 0
	}

	relative := strings.TrimPrefix(name, root+"/")

	return relative, strings.Count(relative, "/") + 1
}

func renderListTree(root *listNode, budget, maxItems int, truncated bool) (string, bool) {
	cutoff := ""
	if truncated {
		cutoff = fmt.Sprintf(
			"\nNote: there are more than %d items in the directory, so not all files may be shown.\n",
			maxItems,
		)
	}

	if len(root.files) == 0 && len(root.subdirs) == 0 {
		return cutoff, false
	}

	root.expanded = true
	rootExpanded := root.renderExpanded()
	if len(rootExpanded) > budget {
		body := renderTruncatedListRoot(root, budget)
		body += "    ...\n\n" + listRootNoticeText

		return body + cutoff, true
	}

	remaining := budget - len(rootExpanded)
	queue := append(make([]string, 0, len(root.subdirs)), root.subdirs...)

	for len(queue) > 0 {
		nodePath := queue[0]
		queue = queue[1:]

		node := navigateList(root, strings.Split(strings.TrimSuffix(nodePath, "/"), "/"))
		if node == nil {
			continue
		}

		node.expanded = true
		expanded := node.renderExpanded()
		summaryCost := node.summaryCharCost()
		if len(expanded) > remaining+summaryCost {
			node.expanded = false

			continue
		}

		remaining += summaryCost
		remaining -= len(expanded)

		for _, child := range node.subdirs {
			queue = append(queue, nodePath+child)
		}
	}

	return root.renderExpanded() + cutoff, false
}

func navigateList(root *listNode, keys []string) *listNode {
	node := root
	for _, key := range keys {
		if key == "" {
			continue
		}
		child := node.children[key+"/"]
		if child == nil {
			return nil
		}
		node = child
	}

	return node
}

func renderTruncatedListRoot(root *listNode, budget int) string {
	var out strings.Builder

	remaining := budget
	childIndent := strings.Repeat("  ", root.depth+2)
	for _, name := range root.allSubitems() {
		var chunk strings.Builder
		chunk.WriteString(root.subitemLine(name))
		chunk.WriteByte('\n')
		if child := root.children[name]; child != nil {
			if summary := child.summary(); summary != "" {
				chunk.WriteString(childIndent)
				chunk.WriteString(summary)
				chunk.WriteByte('\n')
			}
		}
		if chunk.Len() > remaining {
			break
		}
		out.WriteString(chunk.String())
		remaining -= chunk.Len()
	}

	return out.String()
}

func inspectListPath(tree *workspace.Tree, name string) (string, fs.FileInfo, error) {
	normalized, err := tree.NormalizeUserPath(name, true)
	if err != nil {
		return "", nil, err
	}

	if normalized == "." {
		info, statErr := tree.Lstat(".")
		if statErr != nil {
			return "", nil, statErr
		}
		if !info.IsDir() {
			return "", nil, fmt.Errorf("%w: workspace root is not a directory", errNotDirectory)
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
				return "", nil, fmt.Errorf("%w: %q", errIsAFile, current)
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

func extensionKey(name string) string {
	base := path.Base(name)
	if strings.HasPrefix(base, ".") && !strings.Contains(base[1:], ".") {
		return "no-ext"
	}

	ext := path.Ext(base)
	if ext == "" {
		return "no-ext"
	}

	return strings.ToLower(strings.TrimPrefix(ext, "."))
}

func asciiLower(value string) string {
	var out []byte
	for index := range len(value) {
		char := value[index]
		if char >= 'A' && char <= 'Z' {
			if out == nil {
				out = []byte(value)
			}
			out[index] = char + ('a' - 'A')
		}
	}
	if out == nil {
		return value
	}

	return string(out)
}

func entryWindow(offsetValue, limitValue *int, maxEntries int) (int, int, error) {
	offset := 0
	if offsetValue != nil {
		offset = *offsetValue
	}

	limit := maxEntries
	if limitValue != nil {
		limit = *limitValue
	}

	if offset < 0 || limit < 1 || limit > maxEntries {
		return 0, 0, fmt.Errorf("%w: offset must be non-negative and limit must be between 1 and %d", errInvalidArgument, maxEntries)
	}

	return offset, limit, nil
}
