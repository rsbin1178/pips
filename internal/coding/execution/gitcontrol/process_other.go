//go:build !darwin && !linux

package gitcontrol

import (
	"context"
	"time"
)

func runProcess(
	context.Context,
	string,
	[]string,
	[]string,
	[]byte,
	int64,
	time.Duration,
) (commandResult, error) {
	return commandResult{}, ErrUnsupported
}
