package console

import (
	"net/http"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
)

type cpaLostWriteVault struct{ *monitorMemoryVault }

func (v cpaLostWriteVault) Save(id string, c credentials.Credentials) error { return nil }

func TestCPAReadbackFailureKeepsPreviouslyCommittedConnection(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	saveCPAFixture(t, s, "https://original.synthetic.invalid")
	previous := s.cpa
	s.monitorVault = cpaLostWriteVault{s.monitorVault.(*monitorMemoryVault)}
	w := frontendRequest(s, http.MethodPost, "/api/cpa/settings", map[string]any{"baseUrl": "https://replacement.synthetic.invalid", "adminApiKey": "synthetic-replacement-secret", "revision": 1}, "test")
	if w.Code != http.StatusInternalServerError || s.cpa.CredentialRef != previous.CredentialRef || s.cpa.BaseURL != previous.BaseURL || s.cpa.key != previous.key || strings.Contains(w.Body.String(), "synthetic-replacement-secret") {
		t.Fatal("unverified credential published", w.Code, w.Body.String())
	}
	// The previously committed reference remains readable after restart.
	restored := &Server{cfg: s.cfg, monitorVault: s.monitorVault}
	restored.initializeCPA()
	if cfg, err := restored.configuredCPA(); err != nil || cfg.BaseURL != previous.BaseURL || cfg.APIKey != previous.key {
		t.Fatal("failed readback damaged previous connection", err)
	}
}
