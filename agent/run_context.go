package agent

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"
)

var runSequence atomic.Uint64

type runMetadataKey struct{}

// RunMetadata identifies one agent invocation and its place in a nested run
// tree. ParentRunID is empty for a root run.
type RunMetadata struct {
	RunID       string
	ParentRunID string
	Agent       string
}

// RunMetadataFromContext returns the current run metadata. Tools and event
// observers can use it to correlate work without depending on a tracing SDK.
func RunMetadataFromContext(ctx context.Context) (RunMetadata, bool) {
	meta, ok := ctx.Value(runMetadataKey{}).(RunMetadata)
	return meta, ok
}

func newRunMetadata(ctx context.Context, name string) RunMetadata {
	meta := RunMetadata{
		RunID: strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
			strconv.FormatUint(runSequence.Add(1), 36),
		Agent: name,
	}
	if parent, ok := RunMetadataFromContext(ctx); ok {
		meta.ParentRunID = parent.RunID
	}

	return meta
}

func withRunMetadata(ctx context.Context, meta RunMetadata) context.Context {
	return context.WithValue(ctx, runMetadataKey{}, meta)
}
