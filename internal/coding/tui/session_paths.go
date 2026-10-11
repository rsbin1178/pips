package tui

import (
	"path/filepath"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/session"
)

// sessionStateDir resolves one stored Session's derived-state directory from the
// store root. It prefers the known workspace path and falls back to locating the
// Session's group, so no caller joins session paths itself.
func (m *Model) sessionStateDir(workspacePath, sessionID string) string {
	storeRoot := strings.TrimSpace(m.options.SessionsDirectory)
	if storeRoot == "" || strings.TrimSpace(sessionID) == "" {
		return ""
	}

	store, err := session.NewStoreRoot(storeRoot)
	if err != nil {
		return ""
	}

	if strings.TrimSpace(workspacePath) != "" {
		return store.Layout().StateDir(workspacePath, sessionID)
	}

	group, found, err := store.FindSession(sessionID)
	if err != nil || !found {
		return ""
	}

	return filepath.Join(group.SessionDirOf(sessionID), session.StateDirName)
}
