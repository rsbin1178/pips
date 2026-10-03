package coding

import (
	"time"

	"github.com/rsbin1178/pips/internal/coding/compaction"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
)

func (r *Runtime) fullCompactionPolicy() *compaction.Policy {
	policy, reason := effectiveFullCompactionPolicy(r.config.Compaction, r.resolved)
	if reason != "" {
		return nil
	}

	return &policy
}

// The dispatcher fills the chosen child's window and output reserve. Copying
// the parent's resolved output reserve would reject valid smaller child models.
func (r *Runtime) childScopeCompactionPolicy() *compaction.Policy {
	if r.fullCompactionPolicy() == nil {
		return nil
	}

	policy := baseFullCompactionPolicy(r.config.Compaction)

	return &policy
}

func baseFullCompactionPolicy(configured config.CompactionConfig) compaction.Policy {
	return compaction.Policy{
		ReserveTokens:    configured.ReserveTokens,
		ThresholdPercent: configured.ThresholdPercent,
		MaxOutputTokens:  configured.SummaryMaxTokens,
		MinSummaryChars:  configured.MinSummaryChars,
		AttemptsPerStage: configured.AttemptsPerStage,
		RetryDelay:       time.Duration(configured.RetryDelaySeconds) * time.Second,
		Timeout:          time.Duration(configured.TimeoutSeconds) * time.Second,
	}
}

func effectiveFullCompactionPolicy(
	configured config.CompactionConfig,
	resolved modelcatalog.ResolvedModel,
) (compaction.Policy, string) {
	if !configured.Enabled {
		return compaction.Policy{}, "automatic and manual compaction are disabled"
	}

	if resolved.Limits.ContextWindow <= 0 {
		return compaction.Policy{}, "the selected model has no context_window metadata"
	}

	policy := baseFullCompactionPolicy(configured)

	policy.ContextWindow = resolved.Limits.ContextWindow
	if resolved.Options.MaxOutputTokens != nil {
		policy.ReserveTokens = max(policy.ReserveTokens, *resolved.Options.MaxOutputTokens)
	}

	if _, err := policy.Threshold(); err != nil {
		return compaction.Policy{}, "compaction policy or output reserve cannot fit the model context window"
	}

	return policy, ""
}
