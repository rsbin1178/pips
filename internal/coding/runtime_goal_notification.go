//nolint:wsl_v5 // Preserve the notification acknowledgment and accounting boundaries.
package coding

import (
	"context"
	"slices"

	"github.com/rsbin1178/pips/internal/coding/subagent"
)

// recordGoalNotificationDelivery follows the existing exact-message durable
// acknowledgment. Terminal child state alone is not sufficient to assess: the
// parent must first consume its completion evidence.
func (r *Runtime) recordGoalNotificationDelivery(ids []string) error {
	c := r.goalControl
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.id == "" || c.ledger.Cleared {
		return nil
	}
	for _, id := range ids {
		if !slices.Contains(c.ledger.DeliveredChildren, id) {
			c.ledger.DeliveredChildren = append(c.ledger.DeliveredChildren, id)
		}
	}
	return c.saveLedger()
}

func (r *Runtime) observeGoalChildUsage(ctx context.Context) error {
	if r.subagents == nil || r.goalControl == nil {
		return nil
	}
	summaries, err := r.subagents.List(ctx)
	if err != nil {
		return err
	}
	c := r.goalControl
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for _, child := range summaries {
		if !slices.Contains(c.ledger.Roots, child.Ownership.RootInteractionID) || !terminalSubagentState(child.State) || slices.Contains(c.ledger.AccountedChildren, child.ChildSessionID) {
			continue
		}
		c.ledger.AccountedChildren = append(c.ledger.AccountedChildren, child.ChildSessionID)
		c.ledger.ChildUsage.Add(child.Usage)
		changed = true
	}
	if changed {
		return c.saveLedger()
	}
	return nil
}

// goalNotificationOwnership is checked before taking Runtime.mu, preserving
// the control-to-runtime lock order used by admission and publication.
func (r *Runtime) goalNotificationOwnership(root string) (owned, revoked bool) {
	c := r.goalControl
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	owned = slices.Contains(c.ledger.Roots, root)
	return owned, slices.Contains(c.ledger.RevokedRoots, root) || owned && c.ledger.Cleared
}

func goalChildUnconsumed(child subagent.Summary, delivered []string) bool {
	return !terminalSubagentState(child.State) ||
		child.Delivery == subagent.DeliveryBackground && !slices.Contains(delivered, child.ChildSessionID)
}
