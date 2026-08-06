//go:build !darwin && !linux

package pluginsupervisor

func syncDirectory(string) error { return nil }
