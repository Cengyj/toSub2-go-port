package console

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/providers"
	"tosub2-go-port/internal/store"
)

type unreadableSMSRestoreVault struct {
	lockedSMSVault
	err error
}

func (v *unreadableSMSRestoreVault) Load(id string) (credentials.Credentials, error) {
	if strings.HasPrefix(id, "sms-job-") {
		return credentials.Credentials{}, v.err
	}
	return v.lockedSMSVault.Load(id)
}

func newPersistedSMSRestoreServer(t *testing.T, st *store.Store, vault credentials.Store, logger *slog.Logger) (*Server, *jobs.Manager, string) {
	t.Helper()
	jm := jobs.NewManager(context.Background(), 1, st, logger)
	if err := jm.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Host: "127.0.0.1", OutputRoot: st.Root, StartupToken: "test", SMSPollIntervalMS: 2, SMSPollTimeoutMS: 2000}
	if vault == nil {
		// Exercise the no-system-store startup path on every host without
		// consulting the test runner's real macOS Keychain or Windows DPAPI.
		cfg.OutputRoot = ""
	}
	srv := NewServer(cfg, jm, embed.FS{}, logger, vault)
	srv.cfg.OutputRoot = st.Root
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := jm.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return srv, jm, httpServer.URL
}

func smsRestorePublicJobs(t *testing.T, base string) (map[string]map[string]any, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/jobs?pageSize=100", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Console-Token", "test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET jobs failed: status=%d err=%v", resp.StatusCode, err)
	}
	var data struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatal(err)
	}
	result := map[string]map[string]any{}
	for _, job := range data.Jobs {
		result[job["id"].(string)] = job
	}
	return result, string(body)
}

func TestSMSMissingOrInvalidSecureOrderRestoresSafeHTTPState(t *testing.T) {
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	const secret = "sms-private-recovery-canary"
	const phone = "+12025550123"
	statuses := []string{"requesting", "number_acquired", "waiting_sms", "submitting", "submitted", "manual_submitted", "verified", "interrupted", "error", "idle", "completed", "unavailable", ""}
	for _, source := range []string{"nil_store", "missing", "unavailable", "unreadable", "malformed", "identity_mismatch", "empty_lease", "missing_number", "provider_mismatch", "unknown_provider", "missing_custom", "invalid_custom", "invalid_action"} {
		t.Run(source, func(t *testing.T) {
			var calls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer remote.Close()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			memory := &lockedSMSVault{values: map[string]credentials.Credentials{}}
			var vault credentials.Store = memory
			switch source {
			case "nil_store":
				vault = nil
			case "unavailable":
				vault = &unreadableSMSRestoreVault{lockedSMSVault: lockedSMSVault{values: map[string]credentials.Credentials{}}, err: credentials.ErrUnavailable}
			case "unreadable":
				vault = &unreadableSMSRestoreVault{lockedSMSVault: lockedSMSVault{values: map[string]credentials.Credentials{}}, err: errors.New("locked vault: " + secret)}
			}
			provider := "smsbower"
			if source == "unknown_provider" {
				provider = "unknown"
			} else if source == "missing_custom" || source == "invalid_custom" {
				provider = "custom"
			}
			var records []map[string]any
			for i, status := range statuses {
				id, email := fmt.Sprintf("restored-%02d", i), fmt.Sprintf("owner-%02d@example.test", i)
				records = append(records, map[string]any{"id": id, "email": email, "status": "phone_otp", "current_phone": phone, "sms_provider_id": provider, "sms_provider_name": smsProviderName(provider), "sms_status": status})
				state := privateSMSState{Version: 1, JobID: id, Email: email, Provider: provider, APIKey: secret, Base: remote.URL, Service: "dr", Country: "1", Lease: providers.NumberLease{ID: id, Provider: provider, Number: phone}}
				switch source {
				case "identity_mismatch":
					state.Email = "unrelated@example.test"
				case "empty_lease":
					state.Lease.ID = ""
				case "missing_number":
					state.Lease.Number = ""
				case "provider_mismatch":
					state.Lease.Provider = "luban"
				case "invalid_custom":
					state.Custom = &providers.CustomPoolState{Entries: []string{"invalid-pool-" + secret}}
				case "invalid_action":
					state.Action = "invalid-" + secret
				}
				data, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if source == "malformed" {
					data = []byte("invalid-json:" + secret)
				}
				if source != "missing" {
					memory.values[smsCredentialID(id)] = credentials.Credentials{Password: string(data)}
				}
			}
			if err := st.WriteJSON(context.Background(), "job-meta.json", map[string]any{"jobs": records}); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			srv, _, base := newPersistedSMSRestoreServer(t, st, vault, slog.New(slog.NewTextHandler(&logs, nil)))
			public, body := smsRestorePublicJobs(t, base)
			if len(public) != len(statuses) {
				t.Fatalf("restored %d public jobs, expected %d", len(public), len(statuses))
			}
			var persisted struct {
				Jobs []map[string]any `json:"jobs"`
			}
			if err := st.ReadJSON(context.Background(), "job-meta.json", &persisted); err != nil {
				t.Fatal(err)
			}
			byID := map[string]map[string]any{}
			for _, record := range persisted.Jobs {
				byID[record["id"].(string)] = record
			}
			for i, status := range statuses {
				id := fmt.Sprintf("restored-%02d", i)
				job, stored := public[id], byID[id]
				if job["currentPhone"] != phone || stored["current_phone"] != phone {
					t.Errorf("%s: manual fallback phone was lost", status)
				}
				if status == "" || status == "idle" || status == "completed" || status == "unavailable" {
					if job["smsStatus"] != status || job["smsError"] != "" || stored["sms_status"] != status || stored["sms_error"] != "" {
						t.Errorf("%s: inactive SMS state changed or gained a false warning", status)
					}
					continue
				}
				if job["smsStatus"] != "error" || stored["sms_status"] != "error" {
					t.Errorf("%s: missing lease still appears active: %v", status, job["smsStatus"])
				}
				message, _ := job["smsError"].(string)
				if !strings.Contains(message, "自动收短信已停止") || !strings.Contains(message, "手动输入验证码或换号") || stored["sms_error"] != message {
					t.Errorf("%s: safe manual recovery warning is missing or not durable", status)
				}
			}
			srv.smsMu.Lock()
			leases := len(srv.smsLeases)
			srv.smsMu.Unlock()
			if leases != 0 || calls.Load() != 0 {
				t.Fatal("unrecoverable order installed a lease or issued provider HTTP")
			}
			encoded, _ := json.Marshal(persisted)
			if strings.Contains(body, secret) || strings.Contains(string(encoded), secret) || strings.Contains(logs.String(), secret) {
				t.Fatal("private recovery error or record leaked into public state")
			}
		})
	}
}

func TestSMSValidSecureOrderSurvivesManagerAndHTTPServerRestart(t *testing.T) {
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	const id, email, phone = "valid-sms-restart", "owner@example.test", "+12025550123"
	var polls, completed, released atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getStatus":
			polls.Add(1)
			fmt.Fprint(w, "STATUS_OK:654321")
		case "setStatus":
			switch r.URL.Query().Get("status") {
			case "6":
				completed.Add(1)
			case "8":
				released.Add(1)
			}
			fmt.Fprint(w, "ACCESS_READY")
		default:
			t.Error("restored order unexpectedly acquired another number")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer remote.Close()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"id": id, "email": email, "status": "phone_otp", "stage": "sms_otp", "current_phone": phone, "sms_provider_id": "smsbower", "sms_provider_name": "SMSBower", "sms_status": "waiting_sms"}
	if err := st.WriteJSON(context.Background(), "job-meta.json", map[string]any{"jobs": []any{record}}); err != nil {
		t.Fatal(err)
	}
	jobStore, err := store.New(filepath.Join(st.Root, id))
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.WriteJSON(context.Background(), "login-checkpoint.json", map[string]any{"version": 1, "email": email, "stage": "phone_otp", "oauth": map[string]any{"phone": phone}}); err != nil {
		t.Fatal(err)
	}
	state := privateSMSState{Version: 1, JobID: id, Email: email, Provider: "smsbower", APIKey: "fixture-private-key", Base: remote.URL, Service: "dr", ServiceLabel: "Fixture Country", Country: "1", Lease: providers.NumberLease{ID: "existing-order", Provider: "smsbower", Number: phone}}
	data, _ := json.Marshal(state)
	vault := &lockedSMSVault{values: map[string]credentials.Credentials{smsCredentialID(id): {Password: string(data)}}}
	_, jm, base := newPersistedSMSRestoreServer(t, st, vault, slog.New(slog.NewTextHandler(io.Discard, nil)))
	public, _ := smsRestorePublicJobs(t, base)
	if public[id]["smsServiceLabel"] != "Fixture Country" || public[id]["status"] != "resume_available" || public[id]["smsStatus"] != "waiting_sms" || public[id]["smsError"] != "" || public[id]["currentPhone"] != phone {
		t.Fatal("valid lease did not retain its resumable public state")
	}
	if polls.Load() != 0 {
		t.Fatal("restored order polled before checkpoint resumed")
	}
	jm.Emit(jobs.JobEvent{JobID: id, Stage: "sms_otp", Status: jobs.StatusWaitingInput})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if code, err := waitConsoleInput(ctx, jm, id, "phone_otp"); err != nil || code != "654321" {
		t.Fatal("resumed secure order did not deliver its code")
	}
	jm.Emit(jobs.JobEvent{JobID: id, Stage: "phone_verified", Status: jobs.StatusRunning})
	waitConsoleState(t, jm, id, func(j *jobs.Job) bool { return j.SMSStatus == "completed" })
	public, _ = smsRestorePublicJobs(t, base)
	if public[id]["smsServiceLabel"] != "Fixture Country" || public[id]["smsStatus"] != "completed" || public[id]["smsError"] != "" || completed.Load() != 1 || released.Load() != 0 || polls.Load() == 0 {
		t.Fatal("restored order did not complete exactly once through the public state")
	}
}
