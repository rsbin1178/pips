//go:build linux && !amd64 && !arm64

package execution

func platformBackend() backend { return unavailableBackend{} }
