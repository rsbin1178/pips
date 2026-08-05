//nolint:wsl_v5 // Theme application keeps component refreshes in one boundary.
package tui

import (
	"errors"
	"strings"

	"github.com/rsbin/pips/internal/coding/config"
)

var errThemePersistenceUnavailable = errors.New("theme persistence is unavailable")

func (m *Model) loadInitialTheme(selection string) {
	m.themeRegistry = loadThemeRegistry(m.options.ThemeDirectory)
	m.themeDiagnostics = m.themeRegistry.DisplayDiagnostics()
	m.themeSelection = config.NormalizeThemeSelection(selection)

	if m.themeSelection != config.ThemeAuto {
		if theme, ok := m.themeRegistry.Resolve(m.themeSelection); ok {
			m.applyTheme(theme)

			return
		}

		m.themeDiagnostics = appendThemeDiagnostic(
			m.themeDiagnostics,
			themeDiagnostic{category: themeDiagnosticSelection, count: 1},
		)
		m.themeSelection = config.ThemeAuto
	}
	m.applyTheme(m.themeForSelection(config.ThemeAuto))
}

func (m *Model) themeForSelection(selection string) colorTheme {
	dark := m.themeIsDark
	if !m.themeBackgroundKnown {
		dark = true
	}
	if theme, ok := resolvedTheme(m.themeRegistry, selection, dark); ok {
		return theme
	}

	return m.theme
}

func resolvedTheme(registry themeRegistry, selection string, dark bool) (colorTheme, bool) {
	if strings.TrimSpace(selection) == config.ThemeAuto || strings.TrimSpace(selection) == "" {
		if dark {
			return themeDark, true
		}

		return themeLight, true
	}

	return registry.Resolve(selection)
}

func (m *Model) applyTheme(theme colorTheme) {
	if !theme.valid() {
		return
	}
	m.theme = theme
	m.composer.SetStyles(composerStyles(m.theme, m.options.NoColor))
	if m.prompt.kind == promptQuestion {
		m.prompt.question.editor.SetStyles(composerStyles(m.theme, m.options.NoColor))
	}
	if m.prompt.kind == promptPlanReview {
		m.prompt.planReview.editor.SetStyles(composerStyles(m.theme, m.options.NoColor))
	}
	if m.route.kind == routeSessions || m.route.kind == routeSkills {
		m.route.search.SetStyles(sessionSearchStyles(m.theme, m.options.NoColor))
	}
	m.setLayout()
	m.rerenderTranscript(false)
}

func appendThemeDiagnostic(diagnostics []themeDiagnostic, diagnostic themeDiagnostic) []themeDiagnostic {
	for index := range diagnostics {
		if diagnostics[index].category == diagnostic.category {
			diagnostics[index].count += diagnostic.count

			return diagnostics
		}
	}
	if len(diagnostics) < maxThemeDiagnostics {
		return append(diagnostics, diagnostic)
	}
	for index := range diagnostics {
		if diagnostics[index].category == themeDiagnosticOther {
			diagnostics[index].count += diagnostic.count

			return diagnostics
		}
	}

	other := diagnostics[maxThemeDiagnostics-1]
	other.category = themeDiagnosticOther
	other.count += diagnostic.count
	diagnostics[maxThemeDiagnostics-1] = other

	return diagnostics
}
