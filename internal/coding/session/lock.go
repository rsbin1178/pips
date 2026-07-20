package session

import (
	"context"
	"io"
)

type sessionLock interface {
	io.Closer
}

func acquireSessionLock(ctx context.Context, path string) (sessionLock, error) {
	return acquirePlatformLock(ctx, path)
}
