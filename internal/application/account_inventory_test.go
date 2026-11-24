package application

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
)

func TestReadOnlyInventoryKeepsVersionFencesWithoutPrivateHistory(t *testing.T) {
	root := t.TempDir()
	manager := jobs.NewManager(context.Background(), 1, nil, nil)
	j, err := manager.Submit(jobs.Spec{ID: "inventory", Email: "inventory@synthetic.invalid", ImportOnly: true, Input: map[string]any{"password": "synthetic-private-password", "totpSecret": "synthetic-private-seed"}})
	if err != nil {
		t.Fatal(err)
	}
	manager.Emit(jobs.JobEvent{JobID: j.ID, Status: jobs.StatusImported, Prompt: "synthetic-private-history"})
	first := ReadOnlyAccountInventory(root, manager)
	if len(first) != 1 || first[0].ExportRegular {
		t.Fatal("absent export was available")
	}
	data, err := json.Marshal(first)
	if err != nil || strings.Contains(string(data), "synthetic-private") {
		t.Fatal("inventory retained private data", err)
	}
	if err := os.Mkdir(filepath.Join(root, j.ID), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, j.ID, "sub2api-import-oauth.json")
	if err := os.WriteFile(path, []byte(`{"fixture":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	second := ReadOnlyAccountInventory(root, manager)
	if !second[0].ExportRegular || second[0].Revision == first[0].Revision {
		t.Fatal("new export did not change projection version")
	}
	stamp := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	third := ReadOnlyAccountInventory(root, manager)
	if third[0].Revision == second[0].Revision {
		t.Fatal("external file replacement fence was removed")
	}
}
