package jobs

import "path/filepath"

// OAuthGrantPath identifies an already-issued token result for one operation.
// It contains no authentication state. Older results retain their own paths
// and can still be explicitly completed by the CLI without starting a login.
func OAuthGrantPath(root string, job *Job) string {
	name := "oauth-grant.json"
	if job.OAuthResultID != "" {
		if !safeID(job.OAuthResultID) {
			// Callers must reject invalid metadata before file or network work.
			return ""
		}
		name = "oauth-grant-" + job.OAuthResultID + ".json"
	}
	return filepath.Join(root, job.ID, name)
}

func beginOAuthResult(j *Job) {
	switch j.Operation {
	case "resume", "complete-oauth-grant", "complete-2fa":
		return
	}
	j.OAuthResultID = newID()
}
