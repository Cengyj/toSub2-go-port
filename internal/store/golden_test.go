package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
	"tosub2-go-port/internal/sub2api"
)

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "go-port-golden", name))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func goldenObject(t *testing.T, name string) map[string]any {
	t.Helper()
	var data map[string]any
	if e := json.Unmarshal(golden(t, name), &data); e != nil {
		t.Fatal(e)
	}
	return data
}

func TestGoldenStoreAllSixFilesRoundtrip(t *testing.T) {
	for _, name := range []string{"job-meta.json", "login-checkpoint.json", "sub2api-import-oauth.json", "totp-setup-result.json", "password-add-result.json", "sub2api-monitor.json"} {
		t.Run(name, func(t *testing.T) {
			st, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			if e = st.WriteBytes(context.Background(), name, golden(t, name)); e != nil {
				t.Fatal(e)
			}
			var got map[string]any
			if e = st.ReadJSON(context.Background(), name, &got); e != nil {
				t.Fatal(e)
			}
			expected := goldenObject(t, name)
			if !reflect.DeepEqual(expected, got) {
				t.Fatal("legacy JSON changed while reading")
			}
			// Persist an unknown extension without discarding nested legacy fields.
			expected["future_extension"] = map[string]any{"count": float64(7), "list": []any{true, "x"}}
			if e = st.WriteJSON(context.Background(), name, expected); e != nil {
				t.Fatal(e)
			}
			if e = st.ReadJSON(context.Background(), name, &got); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(expected, got) {
				t.Fatal("store rewrote legacy values or unknown fields")
			}
			entries, e := os.ReadDir(st.Root)
			if e != nil {
				t.Fatal(e)
			}
			if len(entries) != 1 || entries[0].Name() != name {
				t.Fatal("temporary atomic write files leaked")
			}
			info, e := os.Stat(st.Path(name))
			if e != nil {
				t.Fatal(e)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatal("compatibility file not private")
			}
		})
	}
}

func TestGoldenMetadataCheckpointAndPrivateResultsRecovery(t *testing.T) {
	root, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	jobID := "golden-resume"
	dir, e := store.New(filepath.Join(root.Root, jobID))
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"job-meta.json", "login-checkpoint.json", "totp-setup-result.json", "password-add-result.json"} {
		if e = dir.WriteBytes(context.Background(), name, golden(t, name)); e != nil {
			t.Fatal(e)
		}
	}
	vault := credentials.NewMemoryStore()
	manager := jobs.NewManager(context.Background(), 1, root, nil)
	manager.SetCredentialStore(vault)
	if e = manager.Restore(context.Background()); e != nil {
		t.Fatal(e)
	}
	job, ok := manager.Get(jobID)
	if !ok || job.Email != "fixture@example.com" || job.Status != "resume_available" || job.Attempts != 2 || !job.HasPassword || !job.HasTOTPKey {
		t.Fatalf("legacy recovery flags changed: present=%v", ok)
	}
	saved, e := vault.Load("fixture@example.com")
	if e != nil {
		t.Fatal(e)
	}
	password := goldenObject(t, "password-add-result.json")["password"].(string)
	secret := goldenObject(t, "totp-setup-result.json")["secret"].(string)
	if saved.Password != password || saved.TOTPSecret != secret {
		t.Fatal("private result values not moved into credential store")
	}
	for _, name := range []string{"totp-setup-result.json", "password-add-result.json"} {
		if _, e = os.Stat(dir.Path(name)); !os.IsNotExist(e) {
			t.Fatal("consumed private result not removed")
		}
	}
	for _, path := range []string{root.Path("job-meta.json"), dir.Path("job-meta.json")} {
		b, e := os.ReadFile(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(b), password) || strings.Contains(string(b), secret) {
			t.Fatal("private golden secret leaked into ordinary metadata")
		}
	}
}

func TestGoldenCompletedOutputRestoresWithoutMetadata(t *testing.T) {
	root, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	dir, e := store.New(filepath.Join(root.Root, "golden-completed"))
	if e != nil {
		t.Fatal(e)
	}
	if e = dir.WriteBytes(context.Background(), "sub2api-import-oauth.json", golden(t, "sub2api-import-oauth.json")); e != nil {
		t.Fatal(e)
	}
	manager := jobs.NewManager(context.Background(), 1, root, nil)
	if e = manager.Restore(context.Background()); e != nil {
		t.Fatal(e)
	}
	job, ok := manager.Get("golden-completed")
	if !ok || !job.ResultSaved || job.Status != jobs.StatusSucceeded || job.Email != "fixture@example.com" {
		t.Fatal("completed golden export was not recovered")
	}
}

func TestGoldenLegacyMonitorConfigurationAndSecretMigration(t *testing.T) {
	dir, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = dir.WriteBytes(context.Background(), "sub2api-monitor.json", golden(t, "sub2api-monitor.json")); e != nil {
		t.Fatal(e)
	}
	monitor := sub2api.NewMonitorService(nil, sub2api.MonitorConfig{}, sub2api.MonitorHooks{}, dir.Path("sub2api-monitor.json"))
	cfg, state := monitor.Configuration(), monitor.PublicState()
	if cfg.BaseURL != "https://sub2api.example" || !cfg.Enabled || !reflect.DeepEqual(cfg.GroupIDs, []int{7}) {
		t.Fatal("legacy nested monitor config was not migrated")
	}
	if state.LastCheckAt == nil || state.LastResult == nil || state.LastResult.Checked != 1 || state.LastResult.Started != 1 {
		t.Fatal("legacy monitor history was not restored")
	}
	if e = monitor.Save(); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(dir.Path("sub2api-monitor.json"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "fixture-admin-key") || strings.Contains(string(b), "adminApiKey") {
		t.Fatal("legacy admin key was re-persisted in ordinary monitor file")
	}
}
