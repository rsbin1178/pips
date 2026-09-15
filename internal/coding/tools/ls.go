package tools

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

const lsName = "ls"

type lsArgs struct {
	Path   *string `json:"path" description:"Optional workspace-relative directory path; defaults to the workspace root"`
	Offset *int    `json:"offset" description:"Optional zero-based entry offset"`
	Limit  *int    `json:"limit" description:"Optional maximum number of entries"`
}

//nolint:gocyclo // Entry typing and pagination share one small bounded rendering loop.
func (s *service) ls(ctx context.Context, args lsArgs) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", failure(lsName, err)
	}

	name := "."
	if args.Path != nil && strings.TrimSpace(*args.Path) != "" {
		name = *args.Path
	}

	normalized, err := inspectTraversalBase(s.tree, name)
	if err != nil {
		return "", failure(lsName, err)
	}

	offset, limit, err := entryWindow(args.Offset, args.Limit, s.limits.ListEntries)
	if err != nil {
		return "", failure(lsName, err)
	}

	entries, err := s.tree.ReadDir(normalized)
	if err != nil {
		return "", failure(lsName, err)
	}

	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

	var body strings.Builder

	returned := 0
	truncated := false
	reason := ""
	nextOffset := 0

	for index := offset; index < len(entries); index++ {
		if err := ctx.Err(); err != nil {
			return "", failure(lsName, err)
		}

		if returned >= limit {
			truncated = true
			reason = "entries"
			nextOffset = index

			break
		}

		entry := entries[index]

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
