//nolint:wsl_v5 // Cost fixtures and their assertions stay adjacent.
package config_test

import (
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCostTableDecodesPricesAndCurrency(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[cost]
currency = "EUR"

[cost.models."anthropic/claude-sonnet-4-5"]
input_per_million_tokens = 3.00
cached_input_per_million_tokens = 0.30
cache_write_per_million_tokens = 3.75
output_per_million_tokens = 15.00

[cost.models."openai/gpt-5"]
input_per_million_tokens = 1.25
output_per_million_tokens = 10.00
`)
	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, "EUR", loaded.Config.Cost.Currency)

	pricing, ok := loaded.Config.Cost.Price("anthropic/claude-sonnet-4-5")
	require.True(t, ok)
	assert.InDelta(t, 3.00, pricing.InputPerMillionTokens, 1e-9)
	assert.InDelta(t, 0.30, pricing.CachedInputPerMillionTokens, 1e-9)
	assert.InDelta(t, 3.75, pricing.CacheWritePerMillionTokens, 1e-9)
	assert.InDelta(t, 15.00, pricing.OutputPerMillionTokens, 1e-9)

	partial, ok := loaded.Config.Cost.Price("openai/gpt-5")
	require.True(t, ok)
	assert.InDelta(t, 1.25, partial.InputPerMillionTokens, 1e-9)
	assert.Zero(t, partial.CachedInputPerMillionTokens, "an absent price counts zero")
	assert.Zero(t, partial.CacheWritePerMillionTokens)

	_, ok = loaded.Config.Cost.Price("missing/model")
	assert.False(t, ok, "a model absent from the table has no configured price")
}

func TestLoadCostTableDefaultsCurrencyToUSD(t *testing.T) {
	t.Parallel()

	defaults, err := config.Load(config.LoadOptions{})
	require.NoError(t, err)
	assert.Equal(t, config.DefaultCostCurrency, defaults.Config.Cost.Currency)
	assert.Empty(t, defaults.Config.Cost.Models)

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[cost.models.\"demo/model\"]\ninput_per_million_tokens = 1.0\n")
	loaded, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)
	assert.Equal(t, "USD", loaded.Config.Cost.Currency)
}

func TestLoadCostTableRejectsBadPricesAndUnknownKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want error
	}{
		{
			name: "negative price",
			body: "[cost.models.\"demo/model\"]\ninput_per_million_tokens = -1.0\n",
			want: config.ErrInvalid,
		},
		{
			name: "non numeric price",
			body: "[cost.models.\"demo/model\"]\ninput_per_million_tokens = \"cheap\"\n",
			want: config.ErrDecode,
		},
		{
			name: "unknown key in the table",
			body: "[cost]\ncurrency = \"USD\"\nunknown = true\n",
			want: config.ErrDecode,
		},
		{
			name: "unknown key in a model entry",
			body: "[cost.models.\"demo/model\"]\ninput_per_million_tokens = 1.0\nunknown = 2.0\n",
			want: config.ErrDecode,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, test.body)
			_, err := config.Load(config.LoadOptions{ConfigFile: path})
			require.Error(t, err)
			require.ErrorIs(t, err, test.want)
		})
	}
}
