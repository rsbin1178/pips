//go:build !darwin && !linux

package teamcontrol

import "os"

func acquireJournalLock(_ *os.File) error { return ErrUnsupported }

func releaseJournalLock(_ *os.File) error { return nil }
