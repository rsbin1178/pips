//go:build !darwin && !linux

package teamworktree

import "os"

func acquireLeaseLock(*os.File) error  { return ErrUnsupported }
func validateLeaseLock(*os.File) error { return ErrUnsupported }
func releaseLeaseLock(*os.File) error  { return nil }
