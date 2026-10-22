// Package heapprofile writes an opt-in runtime heap profile for long-running
// interactive sessions. It is a debugging entry point, not a monitoring system:
// capture happens on demand (SIGUSR2) and once on exit.
package heapprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
)

// EnvPath names the destination file. When it is empty or unset, nothing is
// captured and no signal is registered.
const EnvPath = "PIPS_HEAP_PROFILE"

// Capture writes the retained heap to path and replaces any previous capture.
// A collection runs first so the profile describes live memory instead of
// garbage waiting to be freed.
func Capture(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("heap profile: create directory: %w", err)
	}

	file, err := os.CreateTemp(directory, ".heap-*.tmp")
	if err != nil {
		return fmt.Errorf("heap profile: create file: %w", err)
	}
	tempPath := file.Name()
	committed := false

	defer func() {
		if !committed {
			_ = file.Close()
			_ = os.Remove(tempPath)
		}
	}()

	runtime.GC()

	if err := pprof.WriteHeapProfile(file); err != nil {
		return fmt.Errorf("heap profile: write: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("heap profile: sync: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("heap profile: close: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return errors.Join(
			fmt.Errorf("heap profile: publish: %w", err),
			os.Remove(tempPath),
		)
	}

	committed = true

	return nil
}

// Start installs the environment-driven capture loop. The returned stop
// function halts signal capture and writes one final profile, so a normal exit
// leaves the last retained heap behind.
func Start() func() {
	path := strings.TrimSpace(os.Getenv(EnvPath))
	if path == "" {
		return func() {}
	}

	stop := watchCaptureSignal(func() { _ = Capture(path) })

	return func() {
		stop()
		_ = Capture(path)
	}
}
