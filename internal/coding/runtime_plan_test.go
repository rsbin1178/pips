package coding

import (
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimePlanDocumentPathIsSessionBoundAndDoesNotCreateFile(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("unused")))
	path, err := runtime.PlanDocumentPath()
	require.NoError(t, err)
	assert.Equal(t, runtime.planStore.Path(), path)
	assert.Equal(t, "plan.md", filepath.Base(path))
	assert.Equal(t, runtime.handle.Metadata().ID, filepath.Base(filepath.Dir(path)))

	document, err := runtime.planStore.Read(t.Context())
	require.ErrorIs(t, err, planmode.ErrNotFound)
	assert.Empty(t, document.Content)
}
