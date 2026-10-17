package legacyruntime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGoWorkerReplyPreservesQueuedTerminalDiagnostic(t *testing.T) {
	for _, mode := range []string{"request-exit-failure", "request-exit-unknown-failure", "request-exit-no-failure"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TOSUB2_SCRIPT_BRIDGE_FIXTURE", mode)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			// An unbuffered event queue deliberately leaves the terminal message
			// waiting for its owner while the worker exits and the owner replies.
			events := make(chan runtimeEvent)
			r, err := startProcessRuntime(ctx, executable, []string{"--auth-js-worker", "parent"}, "parent", map[string]any{}, events)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := r.close(); err != nil {
					t.Error(err)
				}
				assertRuntimeInputsRemoved(t, tmp)
			})
			select {
			case event := <-events:
				if event.err != nil || event.message.Type != "request" {
					t.Fatalf("fixture did not issue its request: %+v", event)
				}
			case <-ctx.Done():
				t.Fatal("worker did not issue its request")
			}
			select {
			case <-r.done:
			case <-ctx.Done():
				t.Fatal("worker did not exit before its response")
			}
			err = r.send(ctx, map[string]any{"id": 1, "status": 200, "body": "fixture"})
			if mode == "request-exit-failure" {
				if err == nil || !strings.Contains(err.Error(), "GO_JS_RUNTIME: SCRIPT_EXCEPTION") {
					t.Fatalf("worker exit hid its queued terminal diagnosis: %v", err)
				}
			} else if mode == "request-exit-no-failure" {
				if err == nil || !strings.Contains(err.Error(), "LEGACY_RUNTIME_FAILED") || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("worker exit without a diagnosis was hidden or stalled: %v", err)
				}
			} else if !errors.Is(err, ErrProtocol) {
				t.Fatalf("unknown worker code bypassed the diagnostic allowlist: %v", err)
			}
			if strings.Contains(err.Error(), "fixture-private") || strings.Contains(err.Error(), "fixture.invalid") {
				t.Fatal("worker terminal diagnostic exposed remote data")
			}
			cancel()
			if err := r.send(ctx, map[string]any{"id": 2}); !errors.Is(err, context.Canceled) {
				t.Fatalf("terminal diagnosis replaced caller cancellation: %v", err)
			}
		})
	}
}
