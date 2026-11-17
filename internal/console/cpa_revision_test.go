package console

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestCPASettingsRestoredRevisionCannotWrap(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	saveCPAFixture(t, s, "https://cpa.example.test")
	disk := &store.Store{Root: s.cfg.OutputRoot}
	var saved cpaStoredSettings
	if err := disk.ReadJSON(context.Background(), "cpa-settings.json", &saved); err != nil {
		t.Fatal(err)
	}
	saved.Revision = math.MaxInt64 - 1
	if err := disk.WriteJSON(context.Background(), "cpa-settings.json", saved); err != nil {
		t.Fatal(err)
	}
	s.initializeCPA()
	request := map[string]any{"baseUrl": saved.BaseURL, "revision": int64(math.MaxInt64 - 1), "priority": 7}
	w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", request, "test")
	if w.Code != http.StatusOK || s.publicCPASettings()["revision"] != int64(math.MaxInt64) {
		t.Fatalf("last representable CPA revision was refused: status=%d", w.Code)
	}
	path := filepath.Join(s.cfg.OutputRoot, "cpa-settings.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	request["revision"], request["priority"] = int64(math.MaxInt64), 9
	w = frontendRequest(s, http.MethodPost, "/api/cpa/settings", request, "test")
	if w.Code != http.StatusConflict || s.publicCPASettings()["revision"] != int64(math.MaxInt64) || s.publicCPASettings()["priority"] != 7 {
		t.Fatalf("exhausted revision changed committed CPA settings: status=%d", w.Code)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected update changed durable CPA settings", err)
	}
	restored := &Server{cfg: s.cfg, monitorVault: s.monitorVault}
	restored.initializeCPA()
	cfg, err := restored.configuredCPA()
	if err != nil || cfg.BaseURL != saved.BaseURL || cfg.APIKey != "cpa-private-admin-canary" || cfg.Priority != 7 || restored.cpa.Revision != math.MaxInt64 {
		t.Fatal("restart lost the last committed CPA configuration or credential reference", err)
	}
}
