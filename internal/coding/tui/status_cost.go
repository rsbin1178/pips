//nolint:wsl_v5 // The price arithmetic and its formatting stay adjacent.
package tui

import (
	"strconv"
	"strings"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
)

// costCurrencySymbols are the currencies that print a symbol before the amount.
// Any other code prints after it, so a currency the panel does not know still
// reads unambiguously.
var costCurrencySymbols = map[string]string{
	"USD": "$",
	"EUR": "€",
	"CNY": "¥",
	"JPY": "¥",
}

// modelCost prices one model's usage against the configured table and reports
// whether the model had a configured price. A model with no entry contributes
// exactly zero and is reported unpriced, so a caller can count what it could not
// price rather than presenting a partial sum as complete.
func modelCost(cost config.CostConfig, ref string, usage coding.TokenUsage) (float64, bool) {
	pricing, ok := cost.Price(ref)
	if !ok {
		return 0, false
	}
	// Input already includes the cached classes and output already includes
	// reasoning, so the cached classes are charged in place of their share of the
	// input rate and reasoning is never charged again on top of output.
	uncached := max(0, usage.InputTokens-usage.CachedInputTokens-usage.CacheWriteTokens)
	amount := float64(uncached)*pricing.InputPerMillionTokens +
		float64(usage.CachedInputTokens)*pricing.CachedInputPerMillionTokens +
		float64(usage.CacheWriteTokens)*pricing.CacheWritePerMillionTokens +
		float64(usage.OutputTokens)*pricing.OutputPerMillionTokens

	return amount / 1_000_000, true
}

// modelCostText renders one model's cost, or says it has no configured price so
// the row never reads as a priced zero.
func modelCostText(cost config.CostConfig, ref string, usage coding.TokenUsage) string {
	amount, priced := modelCost(cost, ref, usage)
	if !priced {
		return "unpriced"
	}

	return formatCost(amount, cost.Currency)
}

// formatCost renders one amount. A total below one unit keeps four decimals so a
// cheap session does not read as zero; a symboled currency prints `$0.0042` and
// any other code prints `0.0042 XYZ`.
func formatCost(amount float64, currency string) string {
	text := strconv.FormatFloat(amount, 'f', costDecimals(amount), 64)
	if symbol, ok := costCurrencySymbols[strings.ToUpper(strings.TrimSpace(currency))]; ok {
		return symbol + text
	}
	if code := strings.TrimSpace(currency); code != "" {
		return text + " " + code
	}

	return text
}

// costDecimals keeps four decimals below one unit and two above it.
func costDecimals(amount float64) int {
	if amount < 1 {
		return 4
	}

	return 2
}

// costText pairs a block's total with the number of models it could not price,
// so an install without prices reads zero and says why rather than looking
// complete.
func costText(amount float64, currency string, unpriced int) string {
	text := formatCost(amount, currency)
	if unpriced <= 0 {
		return text
	}

	return text + " · " + costModelCountText(unpriced) + " unpriced"
}

// costModelCountText pluralizes the count of models a block could not price.
func costModelCountText(count int) string {
	if count == 1 {
		return "1 model"
	}

	return strconv.Itoa(count) + " models"
}
