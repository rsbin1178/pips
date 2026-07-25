//nolint:wsl_v5 // Picker action and rendering assertions stay grouped.
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillsCommandOpensInlinePickerAndInsertsExactReference(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.skillSnapshot = testSkillSnapshot()
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 72, Height: 18})

	model.Update(key("/"))
	for _, character := range "skills" {
		model.Update(tea.KeyPressMsg{Text: string(character)})
	}
	_, load := model.Update(key("enter"))
	require.NotNil(t, load)
	model.Update(load())

	assert.Equal(t, pickerSkill, model.picker.kind)
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "$go-review")
	assert.Contains(t, content, "Review Go code · project · agents · 2 resources")
	assert.NotContains(t, content, "$model-only")
	assert.Contains(t, content, "1 diagnostics")
	composerLine := lineContaining(strings.Split(content, "\n"), inputArrow+" $")
	skillLine := lineContaining(strings.Split(content, "\n"), "› $go-review")
	assert.Greater(t, skillLine, composerLine)

	model.Update(key("enter"))
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, "$go-review ", model.composer.Value())
}

func TestDollarPickerFiltersRealComposerTokenAndRestoresDraft(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.skillSnapshot = testSkillSnapshot()
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("please ")

	_, load := model.Update(tea.KeyPressMsg{Text: "$"})
	require.NotNil(t, load)
	model.Update(load())
	assert.Equal(t, pickerSkill, model.picker.kind)
	assert.Equal(t, "please $", model.composer.Value())

	model.Update(tea.KeyPressMsg{Text: "dep"})
	assert.Equal(t, "please $dep", model.composer.Value())
	assert.Equal(t, "dep", model.picker.query)
	assert.Contains(t, ansi.Strip(model.View().Content), "› $deploy")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, "please ", model.composer.Value())
}

func TestDollarPickerLeavesUnknownShellVariableAsPlainText(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.skillSnapshot = testSkillSnapshot()
	model := readyModelWithController(t, controller, true)

	_, load := model.Update(tea.KeyPressMsg{Text: "$"})
	require.NotNil(t, load)
	model.Update(load())
	for _, character := range "HOME" {
		model.Update(tea.KeyPressMsg{Text: string(character)})
	}
	assert.Empty(t, model.filteredSkills())

	model.Update(tea.KeyPressMsg{Text: " "})
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, "$HOME ", model.composer.Value())
}

func TestSkillPickerIgnoresStaleAsyncResult(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.skillSnapshot = testSkillSnapshot()
	model := readyModelWithController(t, controller, true)

	first := model.openSkillPickerForInput("")
	firstMessage := first()
	model.restoreSkillPicker()
	second := model.openSkillPickerForInput("next ")
	require.NotNil(t, second)

	model.Update(firstMessage)
	assert.True(t, model.picker.loading)
	assert.Empty(t, model.picker.skills)
	model.Update(second())
	assert.False(t, model.picker.loading)
	require.NotEmpty(t, model.picker.skills)
}

func testSkillSnapshot() coding.SkillSnapshot {
	return coding.SkillSnapshot{
		Skills: []coding.SkillSummary{
			{
				Name: "go-review", Description: "Review Go code",
				Source: coding.SkillSourceProjectAgents, UserInvocable: true,
				ModelInvocable: true, ResourceCount: 2,
			},
			{
				Name: "deploy", Description: "Deploy safely",
				Source: coding.SkillSourceUserPips, UserInvocable: true,
			},
			{
				Name: "model-only", Description: "Model only",
				Source: coding.SkillSourceExtension, ModelInvocable: true,
			},
		},
		Diagnostics: []coding.SkillDiagnostic{{Code: "skill_invalid"}},
	}
}
