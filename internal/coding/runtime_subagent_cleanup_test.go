package coding

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetainFinishedChildProjectionIsBounded(t *testing.T) {
	t.Parallel()

	runtime := &Runtime{children: make(map[string]*childProjection)}

	for index := range maxRetainedFinishedChildProjections + 2 {
		childSessionID := fmt.Sprintf("child-%d", index)
		runtime.children[childSessionID] = &childProjection{finished: true}
		runtime.retainFinishedChildProjection(childSessionID)
	}

	require.Len(t, runtime.children, maxRetainedFinishedChildProjections)
	assert.NotContains(t, runtime.children, "child-0")
	assert.NotContains(t, runtime.children, "child-1")
	assert.Contains(t, runtime.children, "child-2")
	assert.Contains(t, runtime.children,
		fmt.Sprintf("child-%d", maxRetainedFinishedChildProjections+1))
}

func TestReleaseFinishedChildProjectionKeepsTheOrder(t *testing.T) {
	t.Parallel()

	runtime := &Runtime{children: make(map[string]*childProjection)}
	runtime.retainFinishedChildProjection("first")
	runtime.retainFinishedChildProjection("second")
	runtime.retainFinishedChildProjection("third")

	runtime.releaseFinishedChildProjection("second")
	assert.Equal(t, []string{"first", "third"}, runtime.finishedChildren)
}
