package sub2api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAuthAndUpload(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" {
			t.Fatal("missing auth")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer s.Close()
	var c = New(Config{BaseURL: s.URL, APIKey: "secret"})
	if _, e := c.Upload(context.Background(), map[string]any{"x": 1}); e != nil {
		t.Fatal(e)
	}
}

func TestErrorAccountsPaginationAndGroups(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("status") != "error" {
			t.Fatalf("missing status filter")
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"data":{"items":[{"id":1,"group_ids":[7],"credentials":{"email":"a@example.com"}}],"pages":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":2,"group_ids":[8],"credentials":{"email":"b@example.com"}}],"pages":2}}`))
	}))
	defer s.Close()
	c := New(Config{BaseURL: s.URL, APIKey: "secret"})
	accounts, err := c.ErrorAccounts(context.Background(), []int{7})
	if err != nil || len(accounts) != 1 || AccountEmail(accounts[0]) != "a@example.com" {
		t.Fatalf("accounts=%#v err=%v", accounts, err)
	}
}

func TestAccountEmailReadsPublicMonitorSummary(t *testing.T) {
	if got := AccountEmail(map[string]any{"id": int64(4), "email": "A@example.com"}); got != "a@example.com" {
		t.Fatalf("email=%q", got)
	}
}
