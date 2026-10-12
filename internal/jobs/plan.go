package jobs

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"tosub2-go-port/internal/accountplan"
)

// outputPlanType extracts only a recognized tier from an account's private
// export. Requiring the account identity avoids attaching another account's
// subscription to a restored job.
func outputPlanType(root, id, email string) string {
	for _, name := range []string{"sub2api-import-oauth.json", "sub2api-data.json"} {
		data, err := readPlanExport(filepath.Join(root, id, name))
		if err != nil {
			continue
		}
		var document struct {
			Accounts []map[string]any `json:"accounts"`
		}
		if json.Unmarshal(data, &document) != nil {
			continue
		}
		for _, account := range document.Accounts {
			credentials, _ := account["credentials"].(map[string]any)
			extra, _ := account["extra"].(map[string]any)
			accountEmail, _ := credentials["email"].(string)
			if accountEmail == "" {
				accountEmail, _ = extra["email"].(string)
			}
			if accountEmail == "" {
				accountName, _ := account["name"].(string)
				accountEmail = strings.TrimPrefix(accountName, "oauth---")
			}
			if strings.EqualFold(strings.TrimSpace(accountEmail), strings.TrimSpace(email)) {
				if plan := accountplan.FromAccount(account); plan != "unknown" {
					return plan
				}
			}
		}
	}
	return "unknown"
}

// Plan inspection has a tighter bound than private recovery and never follows
// symlinks. The bounded read also handles files growing after the size check.
func readPlanExport(path string) ([]byte, error) {
	const maxSize = 2 << 20
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSize {
		return nil, errors.New("invalid plan metadata source")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("plan metadata source changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSize {
		return nil, errors.New("plan metadata source is too large")
	}
	return data, nil
}
