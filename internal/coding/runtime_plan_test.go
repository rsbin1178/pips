package coding

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimePlanDocumentPathIsSessionBoundAndDoesNotCreateFile(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("unused")))
	path, err := runtime.PlanDocumentPath()
	require.NoError(t, err)
	assert.Equal(t, runtime.planRef.SessionID+".md", filepath.Base(path))
	_, err = runtime.plans.Read(t.Context(), runtime.planRef)
	require.Error(t, err)
}
