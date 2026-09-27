package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExtractCodeAndCustomPoolLimit(t *testing.T) {
	if ExtractCode(map[string]any{"message": "OpenAI code: 123456"}) != "123456" {
		t.Fatal("code not extracted")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"messages":[]}`)) }))
	defer s.Close()
	p := NewCustomPool([]string{"+8613800000000----" + s.URL})
	if _, e := p.GetNumber(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestSMSBowerPlainTextProtocol(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			w.Write([]byte("ACCESS_NUMBER:abc:+12025550123"))
		case "getStatus":
			w.Write([]byte("STATUS_OK:654321"))
		case "setStatus":
			w.Write([]byte("ACCESS_CANCEL"))
		}
	}))
	defer s.Close()
	p := NewSMSBowerProvider("fixture-key", s.URL, "dr", "1")
	l, e := p.GetNumber(context.Background())
	if e != nil || l.ID != "abc" {
		t.Fatal(l, e)
	}
	c, e := p.GetCode(context.Background(), l)
	if e != nil || c != "654321" {
		t.Fatal(c, e)
	}
	if e = p.Release(context.Background(), l); e != nil {
		t.Fatal(e)
	}
}

func TestCustomPoolAcceptsTextareaAndFiltersOldCode(t *testing.T) {
	var calls int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"messages":[{"id":"old","body":"code 111111","received_at":"2020-01-01T00:00:00Z"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"old","body":"code 111111","received_at":"2020-01-01T00:00:00Z"},{"id":"new","body":"code 222222"}]}`))
	}))
	defer s.Close()
	p := NewCustomPoolValue("+12025550123----" + s.URL)
	l, err := p.GetNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	code, err := p.GetCode(context.Background(), l)
	if err != nil || code != "222222" {
		t.Fatalf("code=%q err=%v", code, err)
	}
}

func TestCustomPoolDeduplicatesPhoneAndCaps(t *testing.T) {
	p := NewCustomPoolValue("+12025550123----https://a.example\n+12025550123----https://b.example")
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) != 1 || !strings.Contains(p.entries[0], "b.example") {
		t.Fatalf("entries=%#v", p.entries)
	}
}

func TestCustomPoolPrivateStatePreservesOldMessageBaseline(t *testing.T) {
	var fresh atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fresh.Load() {
			_, _ = w.Write([]byte(`{"messages":[{"id":"new","text":"code 654321"},{"id":"old","text":"code 123456"}]}`))
		} else {
			_, _ = w.Write([]byte(`{"messages":[{"id":"old","text":"code 123456"}]}`))
		}
	}))
	defer server.Close()
	entries := []string{"+12025550123----" + server.URL}
	original := NewCustomPool(entries)
	lease, err := original.GetNumber(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state := original.PrivateState()
	restored := NewCustomPool(entries)
	if err := restored.RestorePrivateState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.GetCode(context.Background(), lease); !errors.Is(err, ErrWaiting) {
		t.Fatal("old SMS accepted after restore", err)
	}
	fresh.Store(true)
	code, err := restored.GetCode(context.Background(), lease)
	if err != nil || code != "654321" {
		t.Fatal(code, err)
	}
}
