package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUploadUsesManagementProtocolAndReplacesStableFile(t *testing.T) {
	var calls atomic.Int32
	var uploadedName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/proxy/v0/management/auth-files" || r.Header.Get("Authorization") != "Bearer management-secret" || r.Header.Get("Content-Type") != "application/json" || strings.Contains(r.URL.String(), "management-secret") {
			t.Error("invalid management request")
		}
		name := r.URL.Query().Get("name")
		if uploadedName != "" && name != uploadedName {
			t.Error("repeat upload did not address the same auth file")
		}
		uploadedName = name
		var doc codexAuth
		if json.NewDecoder(r.Body).Decode(&doc) != nil || doc.Type != "codex" || doc.AccountID != "workspace-123" {
			t.Error("invalid Codex auth document")
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL + "/proxy/v0/management/", APIKey: "management-secret"})
	account := testAccount()
	for range 2 {
		file, err := BuildAuthFile(account)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Upload(t.Context(), file); err != nil {
			t.Fatal(err)
		}
		account["credentials"].(map[string]any)["access_token"] = "rotated-access"
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d", calls.Load())
	}
}

func TestUploadNeverReplaysAndRequiresConfirmedReceipt(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		unknown bool
	}{
		{"redirect", 307, "management-secret", true},
		{"unauthorized", 401, "access-secret", false},
		{"forbidden", 403, "refresh-secret", false},
		{"validation", 400, "refresh-secret", false},
		{"server-error", 500, "refresh-secret", true},
		{"empty", 200, "", true},
		{"malformed", 200, "invalid-secret-json", true},
		{"partial", 207, `{"status":"partial"}`, true},
		{"contradictory", 200, `{"status":"ok","status":"failed"}`, true},
		{"contradictory-success", 200, `{"status":"ok","error":"private-upstream-error"}`, true},
		{"partial-success", 207, `{"status":"ok"}`, true},
		{"trailing", 200, `{"status":"ok"} {}`, true},
		{"too-large", 200, strings.Repeat("x", 65<<10), true},
		{"lost-response", 0, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if test.status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Location", "/replay")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			file, err := BuildAuthFile(testAccount())
			if err != nil {
				t.Fatal(err)
			}
			err = New(Config{BaseURL: server.URL, APIKey: "management-secret", Timeout: time.Second}).Upload(t.Context(), file)
			if err == nil || IsMutationOutcomeUnknown(err) != test.unknown || errors.Is(err, ErrMutationOutcomeUnknown) != test.unknown || calls.Load() != 1 {
				t.Fatalf("error = %v; unknown = %v; requests = %d", err, IsMutationOutcomeUnknown(err), calls.Load())
			}
			for _, secret := range []string{"management-secret", "access-secret", "refresh-secret", server.URL} {
				if strings.Contains(err.Error(), secret) || strings.Contains(SafeError(err), secret) {
					t.Fatal("error exposed private request or response")
				}
			}
		})
	}
}

func TestUploadLocalRejectionAndUnsentFailureAreNotUnknown(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	file, err := BuildAuthFile(testAccount())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		cfg  Config
		ctx  context.Context
		file AuthFile
	}{
		{Config{BaseURL: server.URL, APIKey: ""}, t.Context(), file},
		{Config{BaseURL: server.URL + "?key=secret", APIKey: "key"}, t.Context(), file},
		{Config{BaseURL: server.URL, APIKey: "key"}, ctx, file},
		{Config{BaseURL: server.URL, APIKey: "key"}, t.Context(), AuthFile{Filename: "../private.json", Data: file.Data}},
		{Config{BaseURL: server.URL, APIKey: "key"}, t.Context(), AuthFile{Filename: file.Filename, Data: []byte(`{"type":"codex","cookies":"secret"}`)}},
	} {
		err := New(test.cfg).Upload(test.ctx, test.file)
		if err == nil || IsMutationOutcomeUnknown(err) {
			t.Fatalf("invalid local input = %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("local validation performed a request")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	err = New(Config{BaseURL: "http://" + address, APIKey: "key", Timeout: time.Second}).Upload(t.Context(), file)
	if err == nil || IsMutationOutcomeUnknown(err) {
		t.Fatalf("proven unsent request = %v", err)
	}
}
