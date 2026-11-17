package providers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/mail"
)

func TestCustomPoolDistinguishesLargeNumericMessageIDs(t *testing.T) {
	var fresh atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "9007199254740992"
		if fresh.Load() {
			id = "9007199254740993"
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":` + id + `,"code":"654321"}]}`))
	}))
	defer server.Close()
	pool := NewCustomPool([]string{"+12025550123----" + server.URL})
	lease, err := pool.GetNumber(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.GetCode(t.Context(), lease); !errors.Is(err, ErrWaiting) {
		t.Fatal("baseline message was accepted", err)
	}
	fresh.Store(true)
	if code, err := pool.GetCode(t.Context(), lease); err != nil || code != "654321" {
		t.Fatalf("a new message sharing the old code was suppressed: code=%q error=%v", code, err)
	}
	if _, err := pool.GetCode(t.Context(), lease); !errors.Is(err, ErrWaiting) {
		t.Fatal("same new message was accepted twice", err)
	}
}

func TestCustomPoolRestoresLegacyLargeNumericMessageBaseline(t *testing.T) {
	var fresh atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fresh.Load() {
			_, _ = w.Write([]byte(`{"messages":[{"id":9007199254740999,"code":"654321"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":9007199254740993,"code":"654321"}]}`))
	}))
	defer server.Close()
	entries := []string{"+12025550123----" + server.URL}
	pool := NewCustomPool(entries)
	lease, err := pool.GetNumber(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state := pool.PrivateState()
	// The old decoder formatted this rounded float64 as a string before
	// hashing. Build the old persisted baseline through that same identity.
	old := mail.ExtractCandidates(`{"id":"9.007199254740992e+15","code":"654321"}`)
	if len(old) != 1 {
		t.Fatal("invalid synthetic baseline")
	}
	stored := state.Leases[lease.ID]
	stored.Seen = map[string]bool{old[0].Key: true}
	state.Leases[lease.ID] = stored
	restored := NewCustomPool(entries)
	if err := restored.RestorePrivateState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.GetCode(t.Context(), lease); !errors.Is(err, ErrWaiting) {
		t.Fatal("legacy baseline was replayed after restore", err)
	}
	fresh.Store(true)
	if code, err := restored.GetCode(t.Context(), lease); err != nil || code != "654321" {
		t.Fatal("legacy baseline blocked a distinct new message", err)
	}
	if _, err := restored.GetCode(t.Context(), lease); !errors.Is(err, ErrWaiting) {
		t.Fatal("restored pool accepted the new message twice", err)
	}
}
