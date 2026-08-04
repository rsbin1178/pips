//nolint:wsl_v5 // Policy normalization and overlap checks are one internal seam.
package execution

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// pathPolicy is the internal C-lite seam shared by native backends. Writable
// roots are explicit resources; protected paths are narrower read-only or
// denied subpaths. The public configuration remains compatible with the
// existing []string protected-path contract.
type pathPolicy struct {
	writableRoots     []string
	protectedSubpaths []string
}

func newPathPolicy(writableRoots, protectedSubpaths []string) pathPolicy {
	return pathPolicy{
		writableRoots:     sortPolicyPaths(writableRoots),
		protectedSubpaths: sortPolicyPaths(protectedSubpaths),
	}
}

// sortPolicyPaths gives broader paths a deterministic first position, then
// orders equal-specificity paths lexically. Native policy compilers can apply
// narrower rules afterwards without depending on caller or map iteration order.
func sortPolicyPaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, value := range paths {
		value = filepath.Clean(value)
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}

	sort.Slice(result, func(i, j int) bool {
		leftDepth := pathDepth(result[i])
		rightDepth := pathDepth(result[j])
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}

		return result[i] < result[j]
	})

	return result
}

func pathDepth(value string) int {
	if value == string(filepath.Separator) {
		return 0
	}

	return strings.Count(filepath.Clean(value), string(filepath.Separator))
}

func validateWritableRootPolicy(writableRoots, protectedSubpaths []string, privateDir string) error {
	owned := false
	for _, root := range writableRoots {
		if root == privateDir || pathContains(root, privateDir) {
			owned = true
			break
		}
	}
	if !owned {
		return fmt.Errorf("%w: private directory is not an explicit writable root", ErrInvalidOperation)
	}

	for _, protected := range protectedSubpaths {
		if protected == string(filepath.Separator) {
			continue
		}

		// A narrower private writable root may reopen a protected ancestor;
		// native backends materialize that specificity explicitly. A protected
		// descendant of the private root is different: allowing the private
		// root would make that deny impossible to preserve, so fail closed.
		if protected == privateDir || pathContains(privateDir, protected) {
			return fmt.Errorf("%w: private directory contains protected path", ErrInvalidOperation)
		}
	}

	return nil
}
