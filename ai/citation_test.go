package ai_test

import (
	"encoding/json"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCitationJSONRoundTrip(t *testing.T) {
	t.Parallel()

	citation := ai.Citation{
		URL:     "https://example.com/article",
		Title:   "Example Article",
		Snippet: "This is an excerpt",
		Index:   1,
		TextRange: &ai.TextRange{
			Start: 10,
			End:   50,
		},
	}

	data, err := json.Marshal(citation)
	require.NoError(t, err)

	var decoded ai.Citation
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, citation, decoded)
}

func TestGroundingMetadataJSONRoundTrip(t *testing.T) {
	t.Parallel()

	metadata := ai.GroundingMetadata{
		WebSearchQueries: []string{"golang 1.26 features", "ai sdk"},
		Raw:              map[string]any{"score": float64(0.95)},
	}

	data, err := json.Marshal(metadata)
	require.NoError(t, err)

	var decoded ai.GroundingMetadata
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, metadata.WebSearchQueries, decoded.WebSearchQueries)
	assert.Equal(t, float64(0.95), decoded.Raw.(map[string]any)["score"])
}
