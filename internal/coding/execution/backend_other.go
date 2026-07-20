//go:build !darwin && !linux

package execution

func platformBackend() backend { return unavailableBackend{} }
