//go:build darwin || linux

package execution

import "strings"

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func sameFileObject(left, right fileObject) bool {
	return left.path == right.path && left.device == right.device && left.inode == right.inode
}
