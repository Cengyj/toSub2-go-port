package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const refreshRecoveryFilename = ".sub2api-import-oauth.json.refresh-grant-recovery.json"
const refreshRecoveryStage = "refresh_output_pending"

// pendingRefreshOperation is only a routing hint. The production runner owns
// all identity, permissions, hash and grant validation before replacing output.
// Even an unreadable or malformed recovery file must prevent a fresh login or
// reuse of an already-rotated refresh token. Its contents never enter Job.
func pendingRefreshOperation(root, id, previous string) string {
	if root == "" || !safeID(id) {
		return ""
	}
	_, err := os.Lstat(filepath.Join(root, id, refreshRecoveryFilename))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	switch op := strings.ToLower(strings.TrimSpace(previous)); op {
	case "refresh", "regenerate", "reauthorize":
		return op
	default:
		return "refresh"
	}
}

func (m *Manager) pendingRefreshOperation(job *Job) string {
	if m.store == nil || job == nil {
		return ""
	}
	return pendingRefreshOperation(m.store.Root, job.ID, job.Operation)
}
