//go:build linux

package execution

func platformBackend() backend { return unavailableBackend{} }
