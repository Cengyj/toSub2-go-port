package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLegacyCustomSMSMailboxRedirectAndBaseline(t *testing.T) {
	var fresh atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mailbox/private-api-fixture" {
			http.Redirect(w, r, "/inbox", http.StatusFound)
			return
		}
		if r.URL.Path != "/inbox" {
			t.Errorf("unexpected synthetic mailbox path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !strings.Contains(r.UserAgent(), "ChatGPT-Onboarding-Console") {
			t.Error("custom SMS requests must preserve the mailbox user agent")
		}
		if fresh.Load() {
			fmt.Fprint(w, `{"messages":[{"id":"new","body":"OpenAI code 654321"},{"id":"old","body":"OpenAI code 123456"}]}`)
		} else {
			fmt.Fprint(w, `{"messages":[{"id":"old","body":"OpenAI code 123456"}]}`)
		}
	}))
	defer server.Close()
	entries := []string{"+12025550123----" + server.URL + "/mailbox/private-api-fixture"}
	pool := NewCustomPool(entries)
	pool.Client = server.Client()
	lease, err := pool.GetNumber(context.Background())
	if err != nil {
		t.Fatalf("legacy custom API redirect rejected during allocation: %v", err)
	}
	if _, err := pool.GetCode(context.Background(), lease); !errors.Is(err, ErrWaiting) {
		t.Fatal("pre-existing code was not excluded from the baseline", err)
	}
	fresh.Store(true)
	if code, err := pool.GetCode(context.Background(), lease); err != nil || code != "654321" {
		t.Fatalf("new code was not read through redirect: code=%q error=%v", code, err)
	}
	restored := NewCustomPool(entries)
	restored.Client = server.Client()
	if err := restored.RestorePrivateState(pool.PrivateState()); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.GetCode(context.Background(), lease); !errors.Is(err, ErrWaiting) {
		t.Fatal("restored custom lease redelivered an observed code", err)
	}
}

func TestCustomSMSMailboxRedirectFailureDoesNotExposeURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/private-api-fixture", http.StatusFound)
	}))
	defer server.Close()
	pool := NewCustomPool([]string{"+12025550123----" + server.URL + "/private-api-fixture"})
	_, err := pool.GetNumber(context.Background())
	if err == nil || IsTerminal(err) || strings.Contains(err.Error(), "private-api-fixture") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("redirect loop must be a safe retryable mailbox failure: %v", err)
	}
	canceled := NewCustomPool([]string{"+12025550123----" + server.URL + "/private-api-fixture"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := canceled.GetNumber(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("custom mailbox cancellation was not preserved: %v", err)
	}
}
