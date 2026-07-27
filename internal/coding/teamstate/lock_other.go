//go:build !darwin && !linux

package teamstate

import "os"

func acquireJournalLock(*os.File) error { return ErrUnsupported }

func releaseJournalLock(*os.File) error { return nil }
