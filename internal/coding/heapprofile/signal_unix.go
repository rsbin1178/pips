//go:build unix

package heapprofile

import (
	"os"
	"os/signal"
	"syscall"
)

// watchCaptureSignal captures on SIGUSR2 until the returned function runs.
func watchCaptureSignal(capture func()) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR2)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-signals:
				capture()
			case <-done:
				return
			}
		}
	}()

	return func() {
		signal.Stop(signals)
		close(done)
	}
}
