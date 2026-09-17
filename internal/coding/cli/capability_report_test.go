package cli

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCapabilityNotesMarkEnforcedFields(t *testing.T) {
	t.Parallel()

	notes := capabilityNotes(ai.CapabilityOverride{
		Vision:           ai.Ptr(true),
		StructuredOutput: ai.Ptr(false),
		Tools:            ai.Ptr(true),
		Reasoning:        ai.Ptr(true),
	})

	require.Len(t, notes, 4)
	assert.Equal(t, capabilityNote{Name: "vision", Value: true, Enforced: true}, notes[0])
	assert.Equal(t, capabilityNote{Name: "tools", Value: true, Enforced: true}, notes[1])
	assert.Equal(t, capabilityNote{Name: "structured_output", Value: false, Enforced: true}, notes[2])
	assert.Equal(t, capabilityNote{Name: "reasoning", Value: true, Enforced: false}, notes[3])
}

func TestCapabilityDoctorLine(t *testing.T) {
	t.Parallel()

	assert.Empty(t, capabilityDoctorLine(ai.CapabilityOverride{}))

	line := capabilityDoctorLine(ai.CapabilityOverride{
		Vision:    ai.Ptr(true),
		Reasoning: ai.Ptr(true),
	})
	assert.Equal(t, "capabilities declared vision reasoning\ncapabilities advisory reasoning", line)

	withWarning := capabilityDoctorLine(ai.CapabilityOverride{Text: ai.Ptr(false)})
	assert.Contains(t, withWarning, "capabilities warning text = false")
}

func TestWriteCapabilityDeclarations(t *testing.T) {
	t.Parallel()

	var builder strings.Builder
	err := writeCapabilityDeclarations(&builder, ai.CapabilityOverride{
		Vision:    ai.Ptr(true),
		Reasoning: ai.Ptr(false),
	})
	require.NoError(t, err)
	assert.Equal(t, "resolved.capabilities.vision = true # declared\n"+
		"resolved.capabilities.reasoning = false # declared advisory\n", builder.String())
}
