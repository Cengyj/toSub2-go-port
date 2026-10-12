package sub2api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func uploadFixtureAccount(email, access string) map[string]any {
	return map[string]any{
		"name": "oauth---" + email, "platform": "openai", "type": "oauth",
		"credentials": map[string]any{"email": email, "access_token": access, "refresh_token": "fixture-new-refresh", "id_token": "fixture-new-id", "chatgpt_account_id": "fixture-new-workspace"},
		"extra":       map[string]any{"codex_fingerprint_mode": "full", "plan_type": "pro"},
	}
}

func uploadFixturePayload(accounts ...map[string]any) map[string]any {
	return map[string]any{"accounts": accounts}
}

func writeUploadFixture(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func TestUploadExistingAccountUpdatesOnlyAuthorization(t *testing.T) {
	local := uploadFixtureAccount(" OWNER@EXAMPLE.TEST ", "fixture-new-access")
	local["credentials"].(map[string]any)["model_mapping"] = map[string]any{"overwritten": "bad"}
	local["credentials"].(map[string]any)["provider_option"] = "must-not-replace"
	local["group_ids"], local["proxy_id"], local["priority"] = []int{99}, 77, 3
	remote := map[string]any{
		"id": 12, "name": "custom existing name", "platform": "openai", "type": "oauth",
		"group_ids": []int{7, 8}, "proxy_id": 9, "priority": 22, "concurrency": 4, "load_factor": 17,
		"status": "disabled", "schedulable": false, "rate_multiplier": 1.7, "notes": "remote notes",
		"extra": map[string]any{"codex_fingerprint_mode": "off", "plan_type": "plus", "custom": map[string]any{"keep": true}},
		"credentials": map[string]any{
			"email": "owner@example.test", "access_token": "fixture-old-access", "refresh_token": "fixture-old-refresh", "id_token": "fixture-old-id", "chatgpt_account_id": "fixture-old-workspace",
			"model_mapping": map[string]any{"gpt-5": "remote-model"}, "provider_option": map[string]any{"keep": true},
		},
	}
	before, _ := json.Marshal(remote)
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if r.Header.Get("x-api-key") != "fixture-admin-key" {
			t.Error("missing authenticated admin request")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			if r.URL.Query().Get("platform") != "openai" || r.URL.Query().Get("type") != "oauth" || r.URL.Query().Get("status") != "" {
				t.Error("lookup must include healthy OAuth accounts")
			}
			writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{remote}, "pages": 1, "total": 1}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
			writeUploadFixture(t, w, map[string]any{"data": remote})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/12":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body) != 1 || body["credentials"] == nil {
				t.Errorf("update must not submit remote configuration: keys=%d", len(body))
			}
			credentials := body["credentials"].(map[string]any)
			for _, key := range []string{"model_mapping", "provider_option"} {
				if !reflect.DeepEqual(credentials[key], remote["credentials"].(map[string]any)[key]) {
					t.Errorf("existing credential configuration %s changed", key)
				}
			}
			for key, value := range map[string]any{"email": "owner@example.test", "access_token": "fixture-new-access", "refresh_token": "fixture-new-refresh", "id_token": "fixture-new-id", "chatgpt_account_id": "fixture-new-workspace"} {
				if credentials[key] != value {
					t.Errorf("authorization field %s not replaced", key)
				}
			}
			writeUploadFixture(t, w, map[string]any{"data": remote})
		default:
			t.Error("existing account attempted creation or lifecycle configuration changes")
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	result, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(context.Background(), uploadFixturePayload(local))
	if err != nil {
		t.Fatal(err)
	}
	got := result.(map[string]any)
	if got["updated"] != float64(1) || got["created"] != float64(0) || got["success"] != float64(1) {
		t.Fatal("existing account upload result counts are wrong")
	}
	if !reflect.DeepEqual(methods, []string{"GET /api/v1/admin/accounts", "GET /api/v1/admin/accounts/12", "PUT /api/v1/admin/accounts/12"}) {
		t.Fatalf("wrong upsert lifecycle: %v", methods)
	}
	after, _ := json.Marshal(remote)
	if string(before) != string(after) {
		t.Fatal("upload mutated the remote detail snapshot")
	}
	public, _ := json.Marshal(PublicUploadResult(result))
	for _, forbidden := range []string{"token", "fixture-new", "fixture-old", "email", "credentials", "provider_option"} {
		if strings.Contains(string(public), forbidden) {
			t.Fatal("upsert result exposed credential or account data")
		}
	}
}

func TestUploadCreatesOnlyAbsentAccountsAndDeduplicates(t *testing.T) {
	var posts atomic.Int32
	first := uploadFixtureAccount("Owner@Example.test", "fixture-first-access")
	last := uploadFixtureAccount(" owner@example.test ", "fixture-last-access")
	groups := []int{7, 9}
	last["group_ids"], last["proxy_id"], last["priority"], last["concurrency"] = groups, 11, 8, 6
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts" {
			writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": []any{}, "pages": 0, "total": 0}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/accounts/batch" {
			t.Error("new account took an unexpected route")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		posts.Add(1)
		if r.Header.Get("Idempotency-Key") != "fixture-idempotency" {
			t.Error("new-account idempotency key was dropped")
		}
		var body struct{ Accounts []map[string]any }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Accounts) != 1 {
			t.Errorf("duplicate local emails created %d records", len(body.Accounts))
		} else {
			created := body.Accounts[0]
			if AccountEmail(created) != "owner@example.test" || created["credentials"].(map[string]any)["access_token"] != "fixture-last-access" || created["proxy_id"] != float64(11) || created["concurrency"] != float64(6) || created["priority"] != float64(8) || !reflect.DeepEqual(created["group_ids"], []any{float64(7), float64(9)}) {
				t.Error("new account configuration or deduplicated authorization changed")
			}
		}
		writeUploadFixture(t, w, map[string]any{"data": map[string]any{"created": 1, "failed": 0, "account_ids": []any{33}, "access_token": "must-not-be-public"}})
	}))
	defer server.Close()
	result, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).UploadWithOptions(context.Background(), uploadFixturePayload(first, last), "fixture-idempotency")
	if err != nil {
		t.Fatal(err)
	}
	got := result.(map[string]any)
	if posts.Load() != 1 || got["created"] != float64(1) || got["updated"] != float64(0) || got["skipped"] != float64(1) || got["total"] != float64(2) {
		t.Fatal("deduplicated upload result counts are wrong")
	}
}

func TestUploadLookupFailuresNeverCreateOrPartiallyUpdate(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"http-failure", `{"access_token":"secret-remote-response"}`, 503},
		{"invalid-json", `invalid secret-remote-response`, 200},
		{"missing-list", `{"data":{"ok":true}}`, 200},
		{"rejected-envelope", `{"ok":false,"data":{"items":[],"pages":1,"total":0}}`, 200},
		{"invalid-record", `{"data":{"items":[null],"pages":1}}`, 200},
		{"invalid-id", `{"data":{"items":[{"id":1.5,"platform":"openai","type":"oauth"}],"pages":1}}`, 200},
		{"missing-records", `{"data":{"items":[],"pages":1,"total":1}}`, 200},
		{"too-many-pages", `{"data":{"items":[],"pages":1001}}`, 200},
		{"wrong-page", `{"data":{"items":[],"page":2,"pages":2}}`, 200},
		{"invalid-pagination", `{"data":{"items":[],"pages":1.5}}`, 200},
		{"ambiguous-email", `{"data":{"items":[{"id":1,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}},{"id":2,"platform":"openai","type":"oauth","credentials":{"email":" OWNER@EXAMPLE.TEST "}}],"pages":1}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				w.WriteHeader(tc.code)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("new@example.test", "fixture-access"), uploadFixtureAccount("owner@example.test", "fixture-access")))
			if err == nil || writes.Load() != 0 || strings.Contains(err.Error(), "secret-remote-response") {
				t.Fatalf("unsafe fallback from failed or ambiguous lookup: %v writes=%d", err, writes.Load())
			}
		})
	}
}

func TestUploadDetailFailuresNeverCreate(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"http-failure", `{"private":"secret-detail"}`, 404},
		{"different-id", `{"data":{"id":13,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}}`, 200},
		{"different-email", `{"data":{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"other@example.test"}}}`, 200},
		{"different-platform", `{"data":{"id":12,"platform":"anthropic","type":"oauth","credentials":{"email":"owner@example.test"}}}`, 200},
		{"different-type", `{"data":{"id":12,"platform":"openai","type":"apikey","credentials":{"email":"owner@example.test"}}}`, 200},
		{"missing-credentials", `{"data":{"id":12,"platform":"openai","type":"oauth","name":"owner@example.test"}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				if r.URL.Path == "/api/v1/admin/accounts" {
					_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"pages":1}}`)
					return
				}
				w.WriteHeader(tc.code)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("new@example.test", "fixture-access"), uploadFixtureAccount("owner@example.test", "fixture-access")))
			if err == nil || writes.Load() != 0 || strings.Contains(err.Error(), "secret-detail") {
				t.Fatal("failed existing-account detail caused a remote write", err)
			}
		})
	}
}

func TestUploadFindsExistingAccountOnLaterPage(t *testing.T) {
	var pages, puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/admin/accounts":
			pages.Add(1)
			if r.URL.Query().Get("page") == "1" {
				_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":1,"platform":"openai","type":"oauth","credentials":{"email":"other@example.test"}}],"pages":2,"total":2}}`)
			} else {
				_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","extra":{"email":"owner@example.test"}}],"pages":2,"total":2}}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
			_, _ = fmt.Fprint(w, `{"data":{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test","model_mapping":{"gpt-5":"kept"}}}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/12":
			puts.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":true}`)
		default:
			t.Error("account beyond first page attempted creation")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-access")))
	if err != nil || pages.Load() != 2 || puts.Load() != 1 {
		t.Fatal("complete pagination failed", err)
	}
}

func TestUploadRejectsRepeatedOrTruncatedPages(t *testing.T) {
	for _, mode := range []string{"repeated-id", "changed-total", "truncated-page"} {
		t.Run(mode, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				if r.URL.Query().Get("page") == "1" {
					_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":1,"platform":"openai","type":"oauth","credentials":{"email":"other@example.test"}}],"pages":2,"total":2}}`)
					return
				}
				switch mode {
				case "repeated-id":
					_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":1,"platform":"openai","type":"oauth","credentials":{"email":"other@example.test"}}],"pages":2,"total":2}}`)
				case "changed-total":
					_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":2,"platform":"openai","type":"oauth","credentials":{"email":"other@example.test"}}],"pages":2,"total":3}}`)
				case "truncated-page":
					_, _ = fmt.Fprint(w, `{"data":{"items":[],"pages":2,"total":2}}`)
				}
			}))
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("new@example.test", "fixture-access")))
			if err == nil || writes.Load() != 0 {
				t.Fatal("incomplete lookup created an account", err)
			}
		})
	}
}

func TestUploadConcurrentClientsDoNotCreateDuplicateAccounts(t *testing.T) {
	var creates, updates atomic.Int32
	var stateMu sync.Mutex
	var remote map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateMu.Lock()
		defer stateMu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			items := []any{}
			if remote != nil {
				items = append(items, remote)
			}
			writeUploadFixture(t, w, map[string]any{"data": map[string]any{"items": items, "total": len(items)}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
			writeUploadFixture(t, w, map[string]any{"data": remote})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/12":
			updates.Add(1)
			writeUploadFixture(t, w, map[string]any{"ok": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/batch":
			creates.Add(1)
			remote = uploadFixtureAccount("owner@example.test", "fixture-access")
			remote["id"] = 12
			writeUploadFixture(t, w, map[string]any{"created": 1})
		default:
			t.Error("unexpected concurrent upsert request")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-admin-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-access")))
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if creates.Load() != 1 || updates.Load() != 1 {
		t.Fatalf("concurrent uploads created=%d updated=%d", creates.Load(), updates.Load())
	}
}

func TestUploadLockWaitCanBeCanceled(t *testing.T) {
	unlock, err := lockBackendUpload(context.Background(), "fixture-backend")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := lockBackendUpload(ctx, "fixture-backend"); err != context.DeadlineExceeded {
		t.Fatal("waiting upload did not obey cancellation", err)
	}
}

func TestUploadPlatformAndAccountTypeArePartOfIdentity(t *testing.T) {
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts" {
			_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":1,"platform":"anthropic","type":"oauth","credentials":{"email":"owner@example.test"}},{"id":2,"platform":"openai","type":"apikey","credentials":{"email":"owner@example.test"}}],"pages":1}}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/batch" {
			creates.Add(1)
			_, _ = fmt.Fprint(w, `{"created":1}`)
			return
		}
		t.Error("unrelated platform or API-key account was updated")
		w.WriteHeader(400)
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-access")))
	if err != nil || creates.Load() != 1 {
		t.Fatal("unrelated account type incorrectly matched", err)
	}
}

func TestUploadResolvesRenamedListAccountWithoutEmail(t *testing.T) {
	var creates, updates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"name":"custom renamed account","platform":"openai","type":"oauth"}],"pages":1}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
			_, _ = fmt.Fprint(w, `{"data":{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test","access_token":"fixture-old-access"}}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/12":
			updates.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":true}`)
		case r.Method == http.MethodPost:
			creates.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-access")))
	if err != nil || creates.Load() != 0 || updates.Load() != 1 {
		t.Fatal("renamed/redacted list account created a duplicate", err)
	}
}

func TestUploadContinuesFullPagesWithoutPaginationMetadata(t *testing.T) {
	var pages, updates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/admin/accounts":
			pages.Add(1)
			if r.URL.Query().Get("page") == "1" {
				items := []any{}
				for id := 1; id <= 100; id++ {
					account := uploadFixtureAccount(fmt.Sprintf("other-%d@example.test", id), "fixture-access")
					account["id"] = id
					items = append(items, account)
				}
				writeUploadFixture(t, w, items)
			} else {
				_, _ = fmt.Fprint(w, `[{"id":101,"platform":"openai","type":"oauth","name":"owner@example.test"}]`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/101":
			_, _ = fmt.Fprint(w, `{"id":101,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/101":
			updates.Add(1)
			_, _ = fmt.Fprint(w, `{"ok":true}`)
		default:
			t.Error("pageless lookup created duplicate or used invalid route")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("owner@example.test", "fixture-access")))
	if err != nil || pages.Load() != 2 || updates.Load() != 1 {
		t.Fatal("full unpaginated response stopped lookup too early", err)
	}
}

func TestUploadFailedUpdateStopsCreationAndIsNeverRetried(t *testing.T) {
	var updates, creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/admin/accounts":
			_, _ = fmt.Fprint(w, `{"data":{"items":[{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}],"pages":1}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/12":
			_, _ = fmt.Fprint(w, `{"data":{"id":12,"platform":"openai","type":"oauth","credentials":{"email":"owner@example.test"}}}`)
		case r.Method == http.MethodPut:
			updates.Add(1)
			w.WriteHeader(503)
			_, _ = fmt.Fprint(w, `{"refresh_token":"secret-mutation-response"}`)
		case r.Method == http.MethodPost:
			creates.Add(1)
			_, _ = fmt.Fprint(w, `{"created":1}`)
		}
	}))
	defer server.Close()
	result, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("new@example.test", "fixture-access"), uploadFixtureAccount("owner@example.test", "fixture-access")))
	if !IsRemoteStatus(err, 503) || updates.Load() != 1 || creates.Load() != 0 || result.(map[string]any)["ok"] != false || strings.Contains(err.Error(), "secret-mutation-response") {
		t.Fatal("failed credential update caused retries, creation, or leaked secrets", err)
	}
}

func TestUploadPartialCreationReportsSafeCountsWithoutRetry(t *testing.T) {
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"data":{"items":[],"pages":0,"total":0}}`)
			return
		}
		creates.Add(1)
		_, _ = fmt.Fprint(w, `{"data":{"success":1,"failed":1,"results":[{"access_token":"secret-created-response"}]}}`)
	}))
	defer server.Close()
	result, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).Upload(context.Background(), uploadFixturePayload(uploadFixtureAccount("first@example.test", "fixture-access"), uploadFixtureAccount("second@example.test", "fixture-access")))
	if err == nil || creates.Load() != 1 {
		t.Fatal("partial upload falsely succeeded or retransmitted", err)
	}
	got := result.(map[string]any)
	if got["created"] != float64(1) || got["failed"] != float64(1) || got["ok"] != false {
		t.Fatal("partial result counts lost")
	}
	public, _ := json.Marshal(PublicUploadResult(got))
	if strings.Contains(string(public), "secret-created-response") || strings.Contains(err.Error(), "secret-created-response") {
		t.Fatal("partial upload result leaked credentials")
	}
}
