package application

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tosub2-go-port/internal/jobs"
)

type AccountInventoryEntry struct {
	jobs.QueryAccount
	Revision      string
	ExportRegular bool
}

// ReadOnlyAccountInventory shares version projection between usage and model
// services. It retains their historical record set and filesystem change
// detection, but does no credential/decryption/network I/O or log cloning.
// The grant's bytes, identity and proxy are still checked under the actual
// operation lease immediately before query and commit.
func ReadOnlyAccountInventory(root string, manager *jobs.Manager) []AccountInventoryEntry {
	list := manager.QueryAccounts()
	out := make([]AccountInventoryEntry, 0, len(list))
	for _, j := range list {
		revision := fmt.Sprintf("%s|%s|%d|%s|%s|%t|%t", j.CreatedAt.UTC().Format(time.RFC3339Nano), j.AuthorizationMethod, j.Attempts, j.LastOperationAt.UTC().Format(time.RFC3339Nano), j.UpdatedAt.UTC().Format(time.RFC3339Nano), j.ResultSaved, j.ProxyConfigured)
		info, err := os.Lstat(filepath.Join(root, j.ID, "sub2api-import-oauth.json"))
		if err == nil {
			revision += fmt.Sprintf("|%d|%d", info.Size(), info.ModTime().UnixNano())
		}
		sum := sha256.Sum256([]byte(revision))
		out = append(out, AccountInventoryEntry{QueryAccount: j, Revision: hex.EncodeToString(sum[:]), ExportRegular: err == nil && info.Mode().IsRegular()})
	}
	return out
}
