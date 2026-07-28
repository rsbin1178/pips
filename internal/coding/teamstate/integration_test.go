package teamstate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIntegrationStateSuccessorIsOneWay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		previous IntegrationState
		next     IntegrationState
		allowed  bool
	}{
		{name: "same", previous: IntegrationReady, next: IntegrationReady, allowed: true},
		{name: "prepare", previous: IntegrationPlanned, next: IntegrationVerified, allowed: true},
		{name: "approve", previous: IntegrationVerified, next: IntegrationApplying, allowed: true},
		{name: "apply", previous: IntegrationApplying, next: IntegrationApplied, allowed: true},
		{name: "reject", previous: IntegrationReady, next: IntegrationRetained, allowed: true},
		{name: "interrupt", previous: IntegrationApplying, next: IntegrationInterrupted, allowed: true},
		{name: "recover", previous: IntegrationInterrupted, next: IntegrationRolledBack, allowed: true},
		{name: "terminal rollback", previous: IntegrationApplied, next: IntegrationReady},
		{name: "skip applying", previous: IntegrationReady, next: IntegrationApplied},
		{name: "retry retained", previous: IntegrationRetained, next: IntegrationApplying},
		{name: "unknown", previous: "unknown", next: IntegrationReady},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.allowed, integrationStateSuccessor(test.previous, test.next))
		})
	}
}
