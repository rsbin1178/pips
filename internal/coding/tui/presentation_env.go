package tui

import "strings"

// environmentValue returns the first value bound to name in an environment slice.
func environmentValue(environment []string, name string) (string, bool) {
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found || key != name {
			continue
		}

		value = strings.TrimSpace(value)
		if value == "" {
			return "", false
		}

		return value, true
	}

	return "", false
}

// hasEnvironmentValue reports whether an environment variable is set and
// non-empty.
func hasEnvironmentValue(environment []string, name string) bool {
	_, ok := environmentValue(environment, name)

	return ok
}

// hasEnvironmentPrefix reports whether an environment variable is set and starts
// with prefix.
func hasEnvironmentPrefix(environment []string, name, prefix string) bool {
	value, ok := environmentValue(environment, name)

	return ok && strings.HasPrefix(value, prefix)
}

// inMultiplexerEnvironment reports whether an environment describes a terminal
// multiplexer session.
func inMultiplexerEnvironment(environment []string) bool {
	if hasEnvironmentValue(environment, "TMUX") || hasEnvironmentValue(environment, "STY") {
		return true
	}

	if hasEnvironmentValue(environment, "ZELLIJ") {
		return true
	}

	return hasEnvironmentPrefix(environment, "TERM", "tmux") ||
		hasEnvironmentPrefix(environment, "TERM", "screen")
}
