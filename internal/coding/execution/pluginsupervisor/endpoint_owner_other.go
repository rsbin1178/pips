//go:build !darwin && !linux

package pluginsupervisor

import "os"

func unixEndpointOwnedByCurrentUser(os.FileInfo) bool { return true }
