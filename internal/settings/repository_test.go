package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/privatestate"
)

func fixture(t *testing.T) (*Repository, credentials.StateProtector) {
	t.Helper()
	p, err := credentials.NewMemoryStateProtector()
	if err != nil {
		t.Fatal(err)
	}
	return New(t.TempDir(), p), p
}

func TestProtectedSettingsRoundTripAndRestart(t *testing.T) {
	r, p := fixture(t)
	ctx := context.Background()
	config := json.RawMessage(`{"adminApiKey":"synthetic-private-value","empty":[],"enabled":false,"zero":0,"vendor":{"huge":9007199254740993}}`)
	for _, scope := range []Scope{SMS, Mail, Sub2API, Proxy} {
		saved, err := r.Save(ctx, scope, 0, config)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(r.path(scope))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("synthetic-private-value")) || !bytes.Contains(raw, []byte(privatestate.Format)) {
			t.Fatal("private configuration was not encrypted")
		}
		reopened, err := New(r.root, p).Load(ctx, scope)
		if err != nil || !equal(saved, reopened) {
			t.Fatalf("restart changed explicit values: %v", err)
		}
	}
}

func TestProtectedSettingsConcurrentBrowsersAndProcessesConflict(t *testing.T) {
	r, p := fixture(t)
	other := New(r.root, p)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, repo := range []*Repository{r, other} {
		wg.Go(func() {
			<-start
			_, err := repo.Save(context.Background(), SMS, 0, json.RawMessage([]string{`{"value":1}`, `{"value":2}`}[i]))
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	// Distinct scopes do not share a revision, and a stale browser cannot clear
	// the other browser's key by uploading its old draft.
	if _, err := other.Save(context.Background(), SMS, 0, json.RawMessage(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := other.Save(context.Background(), Mail, 0, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedSettingsApplicationHoldsSavedGeneration(t *testing.T) {
	r, protector := fixture(t)
	ctx := context.Background()
	other := New(r.root, protector)
	if _, err := r.Save(ctx, Mail, 0, json.RawMessage(`{"method":"GET"}`)); err != nil {
		t.Fatal(err)
	}
	release, err := r.HoldRevision(ctx, Mail, 1)
	if err != nil {
		t.Fatal(err)
	}
	// A second repository represents another browser/process. No newer saved
	// generation can become visible in the check-to-application interval.
	if _, err := other.Save(ctx, Mail, 1, json.RawMessage(`{"method":"POST"}`)); !errors.Is(err, ErrConflict) {
		t.Fatal("save raced with application", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Save(ctx, Mail, 1, json.RawMessage(`{"method":"POST"}`)); err != nil {
		t.Fatal(err)
	}
	if release, err := r.HoldRevision(ctx, Mail, 1); !errors.Is(err, ErrConflict) {
		if release != nil {
			_ = release()
		}
		t.Fatal("stale application accepted", err)
	}
	// A rejected old application must release its lease for the current one.
	release, err = r.HoldRevision(ctx, Mail, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedSettingsUnknownCommitRequiresLocalConfirmation(t *testing.T) {
	r, _ := fixture(t)
	ctx := context.Background()
	writes := 0
	write := r.write
	r.write = func(ctx context.Context, path, purpose, identity, operation string, v any) error {
		writes++
		if err := write(ctx, path, purpose, identity, operation, v); err != nil {
			return err
		}
		return privatefile.ErrDurabilityUncertain
	}
	saved, err := r.Save(ctx, Proxy, 0, json.RawMessage(`{"url":"http://synthetic:private@localhost:8080"}`))
	if !errors.Is(err, ErrUncertain) || saved.Revision != 1 {
		t.Fatalf("receipt=%+v err=%v", saved, err)
	}
	read, err := r.Load(ctx, Proxy)
	if err != nil || read.Revision != 1 {
		t.Fatal(err)
	}
	r.syncDir = func(string) error { return errors.New("synthetic sync error") }
	if _, err := r.Confirm(ctx, Proxy, 1); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	r.syncDir = privatefile.SyncDirectory
	if _, err := r.Confirm(ctx, Proxy, 1); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("confirmation repeated write %d times", writes)
	}
}

func TestProtectedSettingsReadbackFailureNeverConfirmsMigration(t *testing.T) {
	r, _ := fixture(t)
	read := r.read
	reads := 0
	r.read = func(ctx context.Context, path, purpose string, max int64) ([]byte, privatestate.Binding, error) {
		reads++
		if reads > 1 {
			return nil, privatestate.Binding{}, errors.New("synthetic readback failure")
		}
		return read(ctx, path, purpose, max)
	}
	if _, err := r.Save(context.Background(), Mail, 0, json.RawMessage(`{"headers":{"X-Private":"synthetic"}}`)); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
}

func TestProtectedSettingsUnavailableOrInvalidNeverOverwrites(t *testing.T) {
	r, _ := fixture(t)
	ctx := context.Background()
	if _, err := r.Save(ctx, Sub2API, 0, json.RawMessage(`{"key":"synthetic"}`)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(r.path(Sub2API))
	wrong, _ := credentials.NewMemoryStateProtector()
	for _, bad := range []*Repository{New(r.root, wrong), New(r.root, nil)} {
		if _, err := bad.Save(ctx, Sub2API, 1, json.RawMessage(`{}`)); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{"x":1,"x":2}`, `{"x":1} false`} {
		if _, err := r.Save(ctx, Sub2API, 1, json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err := r.Save(ctx, Scope("../bad"), 0, json.RawMessage(`{}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(r.path(Sub2API))
	if !bytes.Equal(before, after) {
		t.Fatal("invalid update replaced protected settings")
	}
}
