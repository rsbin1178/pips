//go:build !darwin && !linux

package execution

import "os/exec"

func configureHookCommand(*exec.Cmd) {}

func cleanupHookCommand(*exec.Cmd) error { return nil }
