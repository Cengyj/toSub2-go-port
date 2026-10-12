package jshost

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/grafana/sobek"
)

func sdkPathFromLoader(source string) string {
	return regexp.MustCompile(`/sentinel/[^\s"'<>]+/sdk\.js`).FindString(source)
}

// This opt-in test executes only public loader/SDK initialization. No token
// method is called, and any HTTP/cookie RPC during initialization fails it.
func TestPublicSDKInitialization(t *testing.T) {
	directory := os.Getenv("TOSUB2_PUBLIC_SDK_FIXTURE")
	if directory == "" {
		t.Skip("set TOSUB2_PUBLIC_SDK_FIXTURE to an anonymously fetched loader.js/sdk.js fixture directory")
	}
	loader, err := os.ReadFile(filepath.Join(directory, "loader.js"))
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := os.ReadFile(filepath.Join(directory, "sdk.js"))
	if err != nil {
		t.Fatal(err)
	}
	// Discover the version from the loader, exactly as the Go parent does. The
	// version is never pinned in production or this test.
	path := sdkPathFromLoader(string(loader))
	if path == "" {
		t.Fatal("version URL not found in public loader")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var output bytes.Buffer
	h := &host{ctx: ctx, kind: "sentinel", input: map[string]any{"url": "https://auth.openai.com/log-in", "html": "<!doctype html><html><head></head><body></body></html>", "userAgent": "Mozilla/5.0 Fixture", "loaderUrl": "https://sentinel.openai.com/backend-api/sentinel/sdk.js", "loaderSource": string(loader), "sdkUrl": "https://sentinel.openai.com" + path, "sdkSource": string(sdk)}, writer: json.NewEncoder(&output), pending: map[int64]func(command) error{}, started: time.Now()}
	defer func() {
		for _, r := range h.realms {
			r.stopInterrupt()
			r.vm.Interrupt("TEST_DONE")
		}
	}()
	if err := h.initialize(); err != nil {
		t.Fatal(err)
	}
	drainTasks(t, h)
	if len(h.realms) != 2 {
		t.Fatalf("expected parent and iframe, got %d contexts", len(h.realms))
	}
	for _, r := range h.realms {
		if len(r.unhandled) > 0 {
			t.Fatal("SDK initialization rejected a promise")
		}
	}
	value := h.root.vm.Get("SentinelSDK")
	if value == nil || sobek.IsUndefined(value) {
		t.Fatal("SDK not exposed")
	}
	for _, method := range []string{"token", "sessionObserverToken"} {
		if _, ok := sobek.AssertFunction(value.ToObject(h.root.vm).Get(method)); !ok {
			t.Fatalf("SDK missing method %s", method)
		}
	}
	if len(h.pending) != 0 {
		t.Fatal("initialization unexpectedly requested network access")
	}
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] != "frame" {
			t.Fatalf("unexpected initialization event %v", event["type"])
		}
	}
}
