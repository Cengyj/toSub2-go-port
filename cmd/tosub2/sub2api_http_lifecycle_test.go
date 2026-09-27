package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"tosub2-go-port/internal/config"
)

type sub2APILifecycleConsole struct {
	cfg    config.Config
	cancel context.CancelFunc
	done   chan struct{}
	err    error // read only after done closes
}

func startSub2APILifecycleConsole(t *testing.T) *sub2APILifecycleConsole {
	t.Helper()
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	t.Setenv("TOSUB2_MAC_CREDENTIAL_ROOT", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	p := &sub2APILifecycleConsole{cfg: lifecycleConfig(t), cancel: cancel, done: make(chan struct{})}
	ready := make(chan struct{})
	go func() {
		p.err = runConsole(ctx, p.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func() { close(ready) })
		close(p.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-p.done:
			if p.err != nil {
				t.Errorf("console shutdown failed: %v", p.err)
			}
		case <-time.After(5 * time.Second):
			t.Error("console shutdown did not finish")
		}
	})
	select {
	case <-ready:
	case <-p.done:
		t.Fatalf("console startup failed: %v", p.err)
	case <-time.After(5 * time.Second):
		t.Fatal("console startup timed out")
	}
	return p
}

func (p *sub2APILifecycleConsole) groups(ctx context.Context, remote string) (*http.Response, error) {
	body, _ := json.Marshal(map[string]any{"config": map[string]any{"baseUrl": remote, "adminApiKey": "synthetic-lifecycle-admin-key"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+p.cfg.Addr()+"/api/sub2api/groups", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Console-Token", p.cfg.StartupToken)
	return (&http.Client{Timeout: 90 * time.Second}).Do(req)
}

func TestConsoleShutdownCancelsForegroundSub2APIRequest(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer remote.Close()
	p := startSub2APILifecycleConsole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		if response, err := p.groups(ctx, remote.URL); err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("foreground request never reached Sub2API")
	}
	p.cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not promptly cancel foreground Sub2API HTTP")
	}
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatalf("shutdown used forced connection closure: %v", p.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground handler blocked shutdown")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("foreground console connection remained open")
	}
}

// The original console allows each remote request 120 seconds. This deliberately
// waits beyond the old Go server's 60-second write timeout, on the real service.
func TestConsoleLongSub2APIResponseKeepsLegacyTimeout(t *testing.T) {
	if os.Getenv("TOSUB2_SLOW_HTTP_TEST") != "1" {
		t.Skip("set TOSUB2_SLOW_HTTP_TEST=1 for a real 65-second Sub2API response")
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/groups/all" || r.Header.Get("X-API-Key") != "synthetic-lifecycle-admin-key" {
			t.Error("Sub2API group request did not preserve its contract")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		select {
		case <-time.After(65 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"data\":[{\"id\":7,\"name\":\"fixture group\",\"status\":\"active\"}]}")
	}))
	defer remote.Close()
	p := startSub2APILifecycleConsole(t)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	started := time.Now()
	response, err := p.groups(ctx, remote.URL)
	if err != nil {
		t.Fatalf("legacy-supported slow response was lost: %v", err)
	}
	defer response.Body.Close()
	var body struct{ Groups []struct{ ID int } }
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != 200 || len(body.Groups) != 1 || body.Groups[0].ID != 7 {
		t.Fatalf("slow response contract failed: status=%d decode=%v", response.StatusCode, err)
	}
	if time.Since(started) < 65*time.Second {
		t.Fatal("test did not traverse the real legacy request window")
	}
	t.Logf("production console returned the slow group response after %s", time.Since(started))
}
