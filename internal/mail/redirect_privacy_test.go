package mail

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestMailboxCrossOriginRedirectDropsAllConfiguredHeaders(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		for _, name := range []string{"X-Mailbox-Token", "ApiKey", "Referer", "Authorization"} {
			if r.Header.Get(name) != "" {
				t.Errorf("cross-origin redirect forwarded configured header %s", name)
			}
		}
		fmt.Fprint(w, `{"code":"123456"}`)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Mailbox-Token") == "" {
			t.Error("initial mailbox query lost configured authentication")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()
	candidates, err := FetchCandidates(t.Context(), origin.URL, RequestConfig{Headers: map[string]string{
		"X-Mailbox-Token": "fixture-private", "ApiKey": "fixture-private", "Referer": "https://fixture.invalid/private", "Authorization": "Bearer fixture-private",
	}}, origin.Client())
	if err != nil || len(candidates) != 1 || calls.Load() != 1 {
		t.Fatalf("safe query redirect failed: candidates=%d calls=%d error=%v", len(candidates), calls.Load(), err)
	}
}

func TestMailboxRedirectNeverForwardsPrivateBodyAcrossOrigins(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, `{"code":"123456"}`)
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, status)
			}))
			defer origin.Close()
			_, err := FetchCandidates(t.Context(), origin.URL, RequestConfig{Method: "POST", Body: `{"access_key":"fixture-private"}`}, origin.Client())
			if err == nil || !IsTerminal(err) || calls.Load() != 0 {
				t.Fatalf("private-body redirect must stop before destination: calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}

func TestMailboxSameOriginRedirectPreservesQueryConfiguration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/inbox", http.StatusTemporaryRedirect)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || string(body) != `{"key":"fixture-private"}` || r.Header.Get("X-Mailbox-Token") != "fixture-private" {
			t.Error("same-origin redirect changed configured mailbox query")
		}
		fmt.Fprint(w, `{"code":"123456"}`)
	}))
	defer server.Close()
	candidates, err := FetchCandidates(t.Context(), server.URL+"/start", RequestConfig{Method: "POST", Body: `{"key":"fixture-private"}`, Headers: map[string]string{"X-Mailbox-Token": "fixture-private"}}, server.Client())
	if err != nil || len(candidates) != 1 {
		t.Fatal("same-origin redirect failed", err)
	}
}
