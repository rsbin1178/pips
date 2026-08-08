//nolint:wsl_v5 // Child-only durable audit validation keeps all fail-closed steps adjacent.
package coding

import (
	"encoding/json"
	"fmt"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
)

const childWorkspaceChangeAuditType = "pips.coding.subagent.workspace-changes/v1alpha1"

type childWorkspaceChangeAudit struct {
	Schema string           `json:"schema"`
	Report WorkspaceChanged `json:"report"`
}

func appendChildWorkspaceChangeAudit(target *harness.Session, report WorkspaceChanged) error {
	if target == nil {
		return fmt.Errorf("%w: child change audit has no session", ErrRuntimeInvalid)
	}
	if err := validateWorkspaceChanged(report); err != nil {
		return fmt.Errorf("%w: child change audit: %w", ErrRuntimeInvalid, err)
	}
	data, err := json.Marshal(childWorkspaceChangeAudit{
		Schema: "pips.coding.subagent.workspace-changes/v1alpha1",
		Report: report,
	})
	if err != nil {
		return fmt.Errorf("coding runtime: encode child change audit: %w", err)
	}
	if _, err := target.AppendCustom(childWorkspaceChangeAuditType, ai.JSON(data)); err != nil {
		return fmt.Errorf("coding runtime: append child change audit: %w", err)
	}

	return nil
}
