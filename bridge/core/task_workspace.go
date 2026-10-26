package core

import (
	"errors"
	"fmt"
	"strings"
)

// taskWorkspaceChannelKey validates that workspace is a selector on the same
// platform as chatSessionKey and returns its platform and channel id. It does
// not check ownership or occupancy: a retry of a committed dispatch must still
// match its receipt after the workspace changes hands, while a new dispatch is
// checked against the live workspace record by resolveTaskWorkspace.
func taskWorkspaceChannelKey(chatSessionKey, workspace string) (platform, wsID string, err error) {
	platform, _, err = parseSessionKeyParts(chatSessionKey)
	if err != nil {
		return "", "", fmt.Errorf("source session: %w", err)
	}
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		return "", "", errors.New("workspace is required")
	}
	if !strings.Contains(ws, ":") {
		ws = platform + ":" + ws
	}
	wsPlatform, wsID, err := parseSessionKeyParts(ws)
	if err != nil || wsPlatform != platform || wsID == "" {
		return "", "", fmt.Errorf("workspace %q is not on platform %s", workspace, platform)
	}
	return platform, wsID, nil
}

// resolveTaskWorkspace validates an explicit execution workspace for a task
// whose source is chatSessionKey's channel. The workspace must already belong
// to that chat (claim it first) and have fewer than teamWorkspaceBusy tasks in
// work. It returns the normalized "<platform>:<workspace id>" channel key.
func (e *Engine) resolveTaskWorkspace(chatSessionKey, workspace string) (string, error) {
	platform, wsID, err := taskWorkspaceChannelKey(chatSessionKey, workspace)
	if err != nil {
		return "", err
	}
	_, chatID, err := parseSessionKeyParts(chatSessionKey)
	if err != nil {
		return "", fmt.Errorf("source session: %w", err)
	}
	parent, ok := e.WorkspaceParent(platform + ":" + wsID)
	if !ok {
		return "", fmt.Errorf("workspace %q is not a recorded workspace of this chat; claim it first", wsID)
	}
	if parent != platform+":"+chatID {
		return "", fmt.Errorf("workspace %q belongs to another chat channel", wsID)
	}
	teamWorkspaceStore.mu.Lock()
	st := loadTeamWorkspacesLocked(teamWorkspacePath(e.dataDir))
	open, _ := teamWorkspaceCountsLocked(st, platform+":"+wsID)
	teamWorkspaceStore.mu.Unlock()
	if open >= teamWorkspaceBusy {
		return "", fmt.Errorf("workspace %q is busy (%d tasks in work)", wsID, open)
	}
	return platform + ":" + wsID, nil
}
