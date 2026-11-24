package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/sub2api"
)

func TestMonitorStartupPreservesV2FileAndAppliesRuntimeDefaults(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	base := "https://monitor.synthetic.invalid"
	reference := monitorCredentialID(base) + "-00112233445566778899aabbccddeeff"
	s.monitorVault.(*monitorMemoryVault).values[reference] = credentials.Credentials{Password: "synthetic-protected-key"}
	// Missing optional fingerprint mode is normalized in memory. Historical
	// values, exact numeric extensions and the original timestamp stay on disk
	// until an explicit configuration change or a new monitor result commits.
	before := []byte(`{"version":2,"enabled":true,"baseUrl":"` + base + `","credentialRef":"` + reference + `","openaiRequestTimezone":"Asia/Shanghai","excludedPlanTypes":[],"updatedAt":"2026-11-01T00:00:00Z","lastError":"previous failed request","permanent":{"blocked@example.test":"old block"},"retained_extension":9007199254740993}`)
	if err := privatefile.Write(t.Context(), s.monitorFile(), before); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		s.initializeMonitor()
		after, err := os.ReadFile(s.monitorFile())
		cfg := s.monitor.Configuration()
		if err != nil || !bytes.Equal(before, after) || cfg.CredentialRef != reference || cfg.CodexFingerprintMode != "session" || cfg.OpenAIRequestTimezone != "Asia/Shanghai" || !cfg.Enabled || s.monitorConfig.APIKey != "synthetic-protected-key" {
			t.Fatal("startup rewrote confirmed state or lost normalized runtime configuration", err)
		}
	}
}

func TestMonitorStartupStillCommitsVerifiedLegacyMigration(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	s.monitorVault = &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	before := []byte(`{"version":1,"enabled":true,"config":{"baseUrl":"https://legacy.synthetic.invalid","adminApiKey":"legacy-synthetic-key","groupIds":[7]}}`)
	if err := privatefile.Write(t.Context(), s.monitorFile(), before); err != nil {
		t.Fatal(err)
	}
	s.initializeMonitor()
	after, err := os.ReadFile(s.monitorFile())
	var saved sub2api.MonitorState
	if err != nil || json.Unmarshal(after, &saved) != nil || saved.Version != 2 || !sub2api.ValidMonitorCredentialReference(saved.BaseURL, saved.CredentialRef) || bytes.Contains(after, []byte("legacy-synthetic-key")) || s.monitorUncertain || !s.monitorKeyPersistent {
		t.Fatal("verified legacy migration was not durably committed", err)
	}
	value, err := s.monitorVault.Load(saved.CredentialRef)
	if err != nil || value.Password != "legacy-synthetic-key" {
		t.Fatal("migration policy does not reference the verified protected key", err)
	}
	s.initializeMonitor()
	restarted, err := os.ReadFile(s.monitorFile())
	if err != nil || !bytes.Equal(after, restarted) || s.monitorConfig.APIKey != "legacy-synthetic-key" {
		t.Fatal("migrated v2 settings changed on the following restart", err)
	}
}

type monitorFaultVault struct {
	*monitorMemoryVault
	saveError error
	wrongRead bool
}

func (v *monitorFaultVault) Save(id string, value credentials.Credentials) error {
	if v.saveError != nil {
		return v.saveError
	}
	return v.monitorMemoryVault.Save(id, value)
}
func (v *monitorFaultVault) Load(id string) (credentials.Credentials, error) {
	if v.wrongRead {
		return credentials.Credentials{Password: "different-synthetic-key"}, nil
	}
	return v.monitorMemoryVault.Load(id)
}

func TestMonitorKeyAndPolicyCommitRetainMatchingGeneration(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-replacement", true: "after-replacement"}[uncertain], func(t *testing.T) {
			s, _ := newMonitorTestServer(t)
			old := sub2api.Config{BaseURL: "https://monitor.synthetic.invalid", APIKey: "old-synthetic-key", GroupIDs: []int{7}}
			if err := s.configureMonitor(old, true, true); err != nil {
				t.Fatal(err)
			}
			oldRef := s.monitor.Configuration().CredentialRef
			before, err := os.ReadFile(s.monitorFile())
			if err != nil {
				t.Fatal(err)
			}
			s.monitorConfigure = func(client *sub2api.Client, cfg sub2api.MonitorConfig, hooks sub2api.MonitorHooks) error {
				if uncertain {
					if err := s.monitor.Configure(client, cfg, hooks); err != nil {
						return err
					}
					return privatefile.ErrDurabilityUncertain
				}
				return errors.New("synthetic replacement failure")
			}
			next := old
			next.APIKey, next.GroupIDs = "new-synthetic-key", []int{9}
			if err := s.configureMonitor(next, true, true); err == nil || errors.Is(err, privatefile.ErrDurabilityUncertain) != uncertain {
				t.Fatal("configuration lost persistence outcome", err)
			}
			vault := s.monitorVault.(*monitorMemoryVault)
			if vault.values[oldRef].Password != old.APIKey || len(vault.values) != 2 {
				t.Fatal("key replacement overwrote or removed the previous generation")
			}
			after, _ := os.ReadFile(s.monitorFile())
			if !uncertain && string(after) != string(before) {
				t.Fatal("definite policy failure changed stored policy")
			}
			s.monitorConfigure = nil
			s.initializeMonitor()
			want := old
			if uncertain {
				want = next
			}
			if s.monitorConfig.APIKey != want.APIKey || len(s.monitorConfig.GroupIDs) != 1 || s.monitorConfig.GroupIDs[0] != want.GroupIDs[0] {
				t.Fatal("restart mixed policy and credential generations")
			}
		})
	}
}

func TestMonitorKeyReadbackFailureDoesNotPublishPolicy(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	old := sub2api.Config{BaseURL: "https://monitor.synthetic.invalid", APIKey: "old-synthetic-key"}
	if err := s.configureMonitor(old, true, true); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.monitorFile())
	s.monitorVault = &monitorFaultVault{monitorMemoryVault: s.monitorVault.(*monitorMemoryVault), wrongRead: true}
	next := old
	next.APIKey = "new-synthetic-key"
	if err := s.configureMonitor(next, true, true); err == nil {
		t.Fatal("unverified protected write was published")
	}
	after, _ := os.ReadFile(s.monitorFile())
	if string(after) != string(before) || s.monitorConfig.APIKey != old.APIKey {
		t.Fatal("failed key readback replaced the previous configuration")
	}
}

func TestMonitorLegacyMigrationPreservesSourceUntilSecureReadback(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	fixture := `{"version":1,"enabled":true,"config":{"baseUrl":"https://legacy.synthetic.invalid","adminApiKey":"legacy-synthetic-key","groupIds":[7]}}`
	if err := os.WriteFile(s.monitorFile(), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	s.monitorVault = &monitorFaultVault{monitorMemoryVault: &monitorMemoryVault{values: map[string]credentials.Credentials{}}, saveError: errors.New("synthetic unavailable")}
	s.initializeMonitor()
	if !s.monitorUncertain || s.monitorKeyPersistent {
		t.Fatal("failed migration reported a persistent key or admitted monitoring")
	}
	if err := s.monitor.Save(); !errors.Is(err, sub2api.ErrLegacyMonitorCredentialPending) {
		t.Fatal("background save could erase the only legacy key", err)
	}
	before, _ := os.ReadFile(s.monitorFile())
	if string(before) != fixture {
		t.Fatal("unconfirmed migration altered the original source")
	}
	s.monitorVault = &monitorMemoryVault{values: map[string]credentials.Credentials{}}
	if err := s.configureMonitorContext(context.Background(), s.monitorConfig, true, false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(s.monitorFile())
	if strings.Contains(string(after), "legacy-synthetic-key") || s.monitorUncertain || !s.monitorKeyPersistent {
		t.Fatal("confirmed migration retained plaintext or failed to restore policy")
	}
	s.initializeMonitor()
	if s.monitorConfig.APIKey != "legacy-synthetic-key" {
		t.Fatal("confirmed migration did not survive restart")
	}
}

func TestMonitorCredentialReferenceDoesNotFallbackToLegacyKey(t *testing.T) {
	for _, reference := range []string{"", "missing"} {
		t.Run(reference, func(t *testing.T) {
			s, _ := newMonitorTestServer(t)
			base := "https://monitor.synthetic.invalid"
			ref := ""
			if reference != "" {
				ref = monitorCredentialID(base) + "-00112233445566778899aabbccddeeff"
			}
			data, _ := json.Marshal(sub2api.MonitorState{Version: 2, BaseURL: base, CredentialRef: ref, Enabled: true})
			if err := os.WriteFile(s.monitorFile(), data, 0600); err != nil {
				t.Fatal(err)
			}
			s.monitorVault.(*monitorMemoryVault).values[monitorCredentialID(base)] = credentials.Credentials{Password: "obsolete-synthetic-key"}
			s.initializeMonitor()
			if s.monitorConfig.APIKey != "" || s.monitorKeyPersistent {
				t.Fatal("new configuration silently reused an unrelated historical key")
			}
		})
	}
}

func TestMonitorLegacyProtectedKeyRemainsReadable(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	base := "https://monitor.synthetic.invalid"
	data, _ := json.Marshal(sub2api.MonitorState{Version: 1, BaseURL: base, Enabled: true})
	if err := os.WriteFile(s.monitorFile(), data, 0600); err != nil {
		t.Fatal(err)
	}
	s.monitorVault.(*monitorMemoryVault).values[monitorCredentialID(base)] = credentials.Credentials{Password: "legacy-protected-key"}
	s.initializeMonitor()
	if s.monitorConfig.APIKey != "legacy-protected-key" || !s.monitorKeyPersistent {
		t.Fatal("legacy protected configuration lost its compatible key binding")
	}
}

func TestMonitorUnreadableSourceCannotBeOverwrittenByEnvironmentOrBackgroundSave(t *testing.T) {
	s, _ := newMonitorTestServer(t)
	t.Setenv("SUB2API_BASE_URL", "https://environment.synthetic.invalid")
	t.Setenv("SUB2API_ADMIN_API_KEY", "synthetic-environment-key")
	for _, invalid := range []string{
		`{broken`,
		`{"version":999,"baseUrl":"https://monitor.synthetic.invalid"}`,
		`{"version":2,"baseUrl":"https://monitor.synthetic.invalid","credentialRef":"some-account@example.test"}`,
		`{"version":2,"baseUrl":"https://monitor.synthetic.invalid","credentialRef":"sub2api-monitor-` + sub2api.BackendIdentity("https://other.synthetic.invalid") + `-00112233445566778899aabbccddeeff"}`,
	} {
		if err := os.WriteFile(s.monitorFile(), []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		s.initializeMonitor()
		s.monitor.SetOperationalError("synthetic diagnostic")
		retained, err := os.ReadFile(s.monitorFile())
		if err != nil || string(retained) != invalid || s.monitorConfig.APIKey != "" || !s.monitorUncertain || !errors.Is(s.monitor.Save(), sub2api.ErrMonitorStateUnavailable) {
			t.Fatal("unreadable source was replaced or admitted with unrelated environment credentials", err)
		}
	}
}
