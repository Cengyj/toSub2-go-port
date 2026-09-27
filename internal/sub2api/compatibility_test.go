package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorPermanentBlockClearsAfterExplicitSuccessfulAuthorization(t *testing.T) {
	for _, action := range []RepairAction{ActionStart, ActionBusy, ActionCooldown, ActionIneligible} {
		t.Run(string(action), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sub2api-monitor.json")
			initial := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path)
			initial.markPermanent("account@example.com", "permanently unavailable")
			restored := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{Evaluate: func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
				return RepairDecision{Action: action, ClearPermanent: true}, nil
			}}, path)
			if decision := restored.decision(context.Background(), "account@example.com", nil); decision.Action != action {
				t.Fatalf("successful local authorization could not clear old block: %+v", decision)
			}
			again := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path)
			if _, exists := again.State.Permanent["account@example.com"]; exists {
				t.Fatal("cleared permanent state returned after restart")
			}
		})
	}
}

func TestMonitorPermanentBlockRequiresExplicitValidEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(context.Context, string, []RemoteAccount) (RepairDecision, error)
	}{
		{"missing-hook", nil},
		{"no-evidence", func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
			return RepairDecision{Action: ActionStart}, nil
		}},
		{"still-blocked", func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
			return RepairDecision{Action: ActionBlocked, ClearPermanent: true}, nil
		}},
		{"missing-job", func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
			return RepairDecision{Action: ActionMissing, ClearPermanent: true}, nil
		}},
		{"unknown-action", func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
			return RepairDecision{Action: "unknown", ClearPermanent: true}, nil
		}},
		{"failed-evaluation", func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
			return RepairDecision{Action: ActionStart, ClearPermanent: true}, errors.New("fixture error")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sub2api-monitor.json")
			initial := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path)
			initial.markPermanent("account@example.com", "permanently unavailable")
			restored := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{Evaluate: tc.hook}, path)
			if decision := restored.decision(context.Background(), "account@example.com", nil); decision.Action != ActionBlocked {
				t.Fatalf("block bypassed without successful local authorization: %+v", decision)
			}
			again := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path)
			if _, exists := again.State.Permanent["account@example.com"]; !exists {
				t.Fatal("permanent state disappeared after restart")
			}
		})
	}
}

func TestMonitorUnblockingDoesNotDiscardCooldown(t *testing.T) {
	s := NewMonitorService(nil, MonitorConfig{Cooldown: time.Hour}, MonitorHooks{Evaluate: func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
		return RepairDecision{Action: ActionStart, ClearPermanent: true}, nil
	}}, filepath.Join(t.TempDir(), "sub2api-monitor.json"))
	s.markPermanent("account@example.com", "unavailable")
	s.markCooldown("account@example.com", "recent repair")
	if d := s.decision(context.Background(), "account@example.com", nil); d.Action != ActionCooldown {
		t.Fatalf("unblocking skipped current cooldown: %+v", d)
	}
	if _, exists := s.State.Permanent["account@example.com"]; exists {
		t.Fatal("successful authorization did not clear obsolete block")
	}
}

func TestProxyOptionsPreserveLegacyDefaultsAndRemoveCredentials(t *testing.T) {
	var data any
	if err := json.Unmarshal([]byte(`[{"id":"12","port":"1080","host":"proxy.example","username":"fixture-user","password":"fixture-secret"}]`), &data); err != nil {
		t.Fatal(err)
	}
	got := sanitizeProxies(data)
	if len(got) != 1 || got[0]["name"] != "代理 12" || got[0]["status"] != "active" || got[0]["port"] != int64(1080) {
		t.Fatalf("legacy proxy defaults missing: %v", got)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "fixture") || strings.Contains(string(b), "password") {
		t.Fatal("proxy credentials returned in option list")
	}
}

func TestMonitorMatchedCountExcludesMissingLocalTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":1,"credentials":{"email":"absent@example.com"}},{"id":2,"credentials":{"email":"present@example.com"}}]`)
	}))
	defer server.Close()
	monitor := NewMonitorService(New(Config{BaseURL: server.URL, APIKey: "fixture-key"}), MonitorConfig{Enabled: true}, MonitorHooks{Evaluate: func(_ context.Context, email string, _ []RemoteAccount) (RepairDecision, error) {
		if email == "absent@example.com" {
			return RepairDecision{Action: ActionMissing}, nil
		}
		return RepairDecision{Action: ActionIneligible}, nil
	}}, "")
	summary, err := monitor.Check(context.Background(), "manual")
	if err != nil || summary.Checked != 2 || summary.Matched != 1 || summary.MissingTask != 1 || summary.Ineligible != 1 {
		t.Fatalf("legacy summary counts changed: %+v %v", summary, err)
	}
}

func TestMonitorCancellationDuringEvaluationIsNotSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":1,"credentials":{"email":"account@example.com"}}]`)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monitor := NewMonitorService(New(Config{BaseURL: server.URL, APIKey: "fixture-key"}), MonitorConfig{Enabled: true}, MonitorHooks{Evaluate: func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
		cancel()
		return RepairDecision{}, context.Canceled
	}}, "")
	summary, err := monitor.Check(ctx, "manual")
	if !errors.Is(err, context.Canceled) || summary.Error != "Sub2API operation canceled" || monitor.Running() {
		t.Fatalf("canceled monitor falsely succeeded: %+v %v", summary, err)
	}
}

func TestErrorAccountsLocallyFiltersPlatformStatusAndStringGroups(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"items":[{"id":1,"platform":"anthropic","status":"error","group_ids":[7]},{"id":2,"platform":"openai","status":"active","group_ids":[7]},{"id":3,"platform":"openai","status":"error","group_ids":["7"]},{"id":4,"account_groups":[{"group_id":"7"}]},{"id":5,"group_ids":[7.5]}],"pages":"1"}}`)
	}))
	defer server.Close()
	got, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).ErrorAccounts(context.Background(), []int{7})
	if err != nil || len(got) != 2 || intValue(got[0]["id"]) != 3 || intValue(got[1]["id"]) != 4 {
		t.Fatalf("unexpected account filter: %v %v", got, err)
	}
}

func TestAccountPublicUnwrapsLegacyEnvelopeAndDropsSecrets(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			account := `{"id":"7","status":"error","credentials":{"email":"ACCOUNT@example.com","access_token":"private-fixture-token"},"extra":{"another_secret":"private"},"group_ids":["3"]}`
			if wrapped {
				account = `{"data":` + account + `}`
			}
			fmt.Fprint(w, account)
		}))
		got, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).AccountPublic(context.Background(), "7")
		server.Close()
		if err != nil || got["id"] != int64(7) || got["email"] != "account@example.com" || !reflect.DeepEqual(got["groupIds"], []int{3}) {
			t.Fatalf("unexpected public account: %v %v", got, err)
		}
		b, _ := json.Marshal(got)
		if strings.Contains(string(b), "private") || strings.Contains(string(b), "token") {
			t.Fatal("credential leaked into public account")
		}
	}
}

func TestAccountIdentityPrefersCredentialEmail(t *testing.T) {
	a := map[string]any{"email": "unrelated@example.com", "name": "another@example.com", "credentials": map[string]any{"email": "  ACCOUNT@example.com  "}}
	if AccountEmail(a) != "account@example.com" {
		t.Fatal("public display email displaced credential identity")
	}
}

func TestRemoteIntegerIDsMustBeExactAndSafe(t *testing.T) {
	for _, value := range []any{1.5, math.Inf(1), math.NaN(), float64(1 << 53), int64(1 << 53), -1, "1.5", "9223372036854775808", json.Number("1.5")} {
		if got := intValue(value); got != 0 {
			t.Errorf("unsafe ID %v became %d", value, got)
		}
	}
	for _, value := range []any{1, int64(1), float64(1), " 1 ", json.Number("1")} {
		if intValue(value) != 1 {
			t.Errorf("valid ID %v rejected", value)
		}
	}
}

func TestSyncAccountCredentialsRejectsWrongRemoteIdentity(t *testing.T) {
	for _, body := range []string{`{"data":{"id":13}}`, `{"data":{"id":12.5}}`, `{"data":{"credentials":{}}}`, `{"data":{"id":12,"credentials":{"email":"other@example.com"}}}`} {
		var writes atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				writes.Add(1)
			}
			fmt.Fprint(w, body)
		}))
		err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).SyncAccountCredentials(context.Background(), 12, map[string]any{"email": "account@example.com", "access_token": "fixture-token"}, "session")
		server.Close()
		if err == nil || writes.Load() != 0 {
			t.Fatal("remote identity mismatch allowed a write", err)
		}
	}
}

func TestSyncAccountCredentialsStopsAtFailedLifecycleStep(t *testing.T) {
	for _, failure := range []string{http.MethodPut, "clear-error", "schedulable"} {
		steps := []string{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			steps = append(steps, r.Method+" "+r.URL.Path)
			if r.Method == failure || strings.HasSuffix(r.URL.Path, "/"+failure) {
				w.WriteHeader(503)
				fmt.Fprint(w, "fixture-secret-response")
				return
			}
			fmt.Fprint(w, `{"data":{"id":12,"credentials":{"email":"account@example.com"}}}`)
		}))
		err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).SyncAccountCredentials(context.Background(), 12, map[string]any{"email": "account@example.com"}, "session")
		server.Close()
		want := map[string]int{http.MethodPut: 2, "clear-error": 3, "schedulable": 4}[failure]
		if !IsRemoteStatus(err, 503) || len(steps) != want || strings.Contains(err.Error(), "secret") {
			t.Fatalf("lifecycle failure did not stop subsequent steps: %v %v", steps, err)
		}
	}
}
