package execution

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
)

const (
	maxPreflightEntries = 100_000
	maxHardlinkFiles    = 256
	preflightTimeout    = 2 * time.Second
)

type preflightResult struct {
	readOnlyFiles []string
}

func preflightWorkspace(ctx context.Context, root string) (preflightResult, error) {
	bounded, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	result := preflightResult{}
	entries := 0

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if err := bounded.Err(); err != nil {
			return err
		}

		entries++
		if entries > maxPreflightEntries {
			return fmt.Errorf("workspace entry limit %d exceeded", maxPreflightEntries)
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		mode := info.Mode()
		if mode.IsDir() || mode&fs.ModeSymlink != 0 {
			return nil
		}

		if !mode.IsRegular() {
			relative, relativeErr := filepath.Rel(root, path)
			if relativeErr != nil {
				relative = "<unknown>"
			}

			return fmt.Errorf("unsupported workspace file type at %q", filepath.ToSlash(relative))
		}

		links, err := fileLinkCount(info)
		if err != nil {
			return err
		}

		if links > 1 {
			result.readOnlyFiles = append(result.readOnlyFiles, path)
			if len(result.readOnlyFiles) > maxHardlinkFiles {
				return fmt.Errorf("workspace hardlink limit %d exceeded", maxHardlinkFiles)
			}
		}

		return nil
	})
	if err != nil {
		return preflightResult{}, fmt.Errorf("coding execution: workspace preflight: %w", err)
	}

	return result, nil
}
