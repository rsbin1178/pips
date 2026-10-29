//go:build !unix

package heapprofile

// watchCaptureSignal is a no-op where SIGUSR2 does not exist.
func watchCaptureSignal(func()) func() {
	return func() {}
}
