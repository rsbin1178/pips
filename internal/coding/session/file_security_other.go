//go:build !darwin && !linux

package session

func secureSessionFile(string) error { return nil }
