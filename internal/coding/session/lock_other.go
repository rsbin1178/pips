//go:build !darwin && !linux

package session

import (
	"context"
	"fmt"
)

func acquirePlatformLock(context.Context, string) (sessionLock, error) {
	return nil, fmt.Errorf("%w: session locking unavailable", ErrUnsupportedPlatform)
}
