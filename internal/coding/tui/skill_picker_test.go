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

func TestSkillsCommandOpensProjectManagerAndTogglesSelection(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.skillSnapshot = testSkillSnapshot()
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 72, Height: 24})

	model.Update(key("/"))
	for _, character := range "skills" {
		model.Update(tea.KeyPressMsg{Text: string(character)})
	}
	_, load := model.Update(key("enter"))
	require.NotNil(t, load)
	driveModelCommands(t, model, load)

	assert.Equal(t, routeSkills, model.route.kind)
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "[x] go-review")
	assert.Contains(t, content, "Review Go code · project · agents · 2 resources")
	assert.Contains(t, content, "model-only")
	assert.NotContains(t, content, "skill_invalid")
	assert.Contains(t, content, "Ctrl+D diagnostics")

	model.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	assert.Contains(t, ansi.Strip(model.View().Content), "skill_invalid")

	_, save := model.Update(key("enter"))
	require.NotNil(t, save)
	model.Update(save())
	assert.Equal(t, routeSkills, model.route.kind)
	assert.Contains(t, ansi.Strip(model.View().Content), "[ ] go-review")
	assert.False(t, controller.skillSnapshot.Skills[0].Enabled)
	assert.Empty(t, model.composer.Value())
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

func TestSkillsManagerFiltersAndRestoresDraftOnEscape(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.skillSnapshot = testSkillSnapshot()
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("keep this draft")
	load := model.openSkillsRoute("keep this draft")
	driveModelCommands(t, model, load)

	model.Update(tea.KeyPressMsg{Text: "model"})
	assert.Equal(t, "model", model.route.search.Value())
	require.Len(t, model.filteredSkillsRouteValues(), 1)
	assert.Equal(t, "model-only", model.filteredSkillsRouteValues()[0].Name)
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "┌")
	assert.Contains(t, content, "model-only")
	assert.NotContains(t, content, "go-review")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, routeNone, model.route.kind)
	assert.Equal(t, "keep this draft", model.composer.Value())
	require.NotNil(t, model.View().Cursor)
}

func TestSkillsManagerExpandsOnlySelectedSkill(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 72, Height: 24})
	model.route = routeState{
		kind: routeSkills, generation: 1,
		search: newRouteSearch(themeDark, true),
	}
	model.route.skills = testSkillSnapshot().Skills
	model.setLayout()

	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "Review Go code · project · agents · 2 resources")
	assert.NotContains(t, content, "Deploy safely")
	assert.NotContains(t, content, "Model only")

	model.Update(key("down"))
	content = ansi.Strip(model.View().Content)
	assert.NotContains(t, content, "Review Go code")
	assert.Contains(t, content, "Deploy safely · user · pips")
	assert.NotContains(t, content, "Model only")
}

func TestDisabledSkillIsAbsentFromDollarPicker(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.skillSnapshot = testSkillSnapshot()
	controller.skillSnapshot.Skills[1].Enabled = false
	model := readyModelWithController(t, controller, true)

	_, load := model.Update(tea.KeyPressMsg{Text: "$"})
	require.NotNil(t, load)
	model.Update(load())
	model.Update(tea.KeyPressMsg{Text: "dep"})

	assert.Empty(t, model.filteredSkills())
	assert.NotContains(t, ansi.Strip(model.View().Content), "$deploy")
}

func TestSkillsManagerFailedToggleKeepsCheckboxAndCanRetry(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.skillSnapshot = testSkillSnapshot()
	controller.skillErr = assert.AnError
	model := readyModelWithController(t, controller, true)
	load := model.openSkillsRoute("")
	driveModelCommands(t, model, load)

	_, save := model.Update(key("enter"))
	require.NotNil(t, save)
	model.Update(save())
	require.ErrorIs(t, model.route.err, assert.AnError)
	assert.True(t, model.route.skills[0].Enabled)
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "Error:")
	assert.Contains(t, content, "[x] go-review")

	controller.skillErr = nil
	_, retry := model.Update(key("enter"))
	require.NotNil(t, retry)
	model.Update(retry())
	require.NoError(t, model.route.err)
	assert.False(t, model.route.skills[0].Enabled)
}

func TestSkillsManagerIgnoresStaleLoadAndFitsNarrowTerminal(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 22, Height: 14})
	model.route = routeState{
		kind: routeSkills, generation: 2,
		search: newRouteSearch(themeDark, true),
	}
	model.setLayout()
	model.Update(skillsRouteDataMsg{
		generation: 1, snapshot: testSkillSnapshot(),
	})
	assert.Empty(t, model.route.skills)

	model.Update(skillsRouteDataMsg{
		generation: 2, snapshot: testSkillSnapshot(),
	})
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "[x] go-review")
	for line := range strings.SplitSeq(content, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), model.width)
	}
	require.NotNil(t, model.View().Cursor)
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
				ID: "skill-review", Name: "go-review", Description: "Review Go code", Enabled: true,
				Source: coding.SkillSourceProjectAgents, UserInvocable: true,
				ModelInvocable: true, ResourceCount: 2,
			},
			{
				ID: "skill-deploy", Name: "deploy", Description: "Deploy safely", Enabled: true,
				Source: coding.SkillSourceUserPips, UserInvocable: true,
			},
			{
				ID: "skill-model", Name: "model-only", Description: "Model only", Enabled: true,
				Source: coding.SkillSourceExtension, ModelInvocable: true,
			},
		},
		Diagnostics: []coding.SkillDiagnostic{{Code: "skill_invalid"}},
	}
}
