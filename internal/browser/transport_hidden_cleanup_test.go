package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func localRequestTargetCDP(t *testing.T, handle func(cdpMessage) (any, *cdpError)) *Session {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			var request cdpMessage
			if connection.ReadJSON(&request) != nil {
				return
			}
			value, failure := handle(request)
			response := cdpMessage{ID: request.ID, Error: failure}
			if value != nil {
				response.Result, _ = json.Marshal(value)
			}
			if connection.WriteJSON(response) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{ws: connection, pending: map[int64]chan cdpResponse{}, authAttempted: map[string]bool{}}
	go session.readLoop(connection)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestHiddenRequestTargetUnsupportedClosesProbeWithoutNavigation(t *testing.T) {
	for _, test := range []struct {
		name      string
		createErr bool
		windowErr bool
	}{
		{name: "hidden_parameter_rejected", createErr: true},
		{name: "hidden_parameter_ignored"},
		{name: "window_proof_unsupported", windowErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			methods := map[string]int{}
			session := localRequestTargetCDP(t, func(request cdpMessage) (any, *cdpError) {
				mu.Lock()
				methods[request.Method]++
				mu.Unlock()
				switch request.Method {
				case "Target.createTarget":
					var options map[string]any
					_ = json.Unmarshal(request.Params, &options)
					if options["hidden"] != true || options["background"] != true || options["url"] != "about:blank" || options["browserContextId"] != nil {
						t.Error("request target was not a hidden target in the original browser context")
					}
					if test.createErr {
						return nil, &cdpError{Code: -32602, Message: "unsupported parameter"}
					}
					return map[string]any{"targetId": "local-probe"}, nil
				case "Browser.getWindowForTarget":
					if test.windowErr {
						return nil, &cdpError{Code: -32601, Message: "unsupported method"}
					}
					return map[string]any{"windowId": 42}, nil
				case "Target.closeTarget":
					return map[string]any{"success": true}, nil
				default:
					t.Errorf("unsupported probe unexpectedly invoked %s", request.Method)
					return map[string]any{}, nil
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			page, targetID, err := session.newPage(ctx)
			if !errors.Is(err, ErrHiddenTargetUnsupported) || page != nil || targetID != "" || session.closed.Load() {
				t.Fatalf("unsupported target did not fail safely: err=%v closed=%v", err, session.closed.Load())
			}
			mu.Lock()
			defer mu.Unlock()
			wantClosed := 1
			if test.createErr {
				wantClosed = 0
			}
			if methods["Target.closeTarget"] != wantClosed || methods["Page.navigate"] != 0 || methods["Target.createTarget"] != 1 {
				t.Fatalf("probe leaked a target or retried a visible tab: method counts=%v", methods)
			}
		})
	}
}

func TestRequestTargetFailedClosureTerminatesOnlyItsOwnedSession(t *testing.T) {
	for _, falseSuccess := range []bool{false, true} {
		t.Run(map[bool]string{false: "protocol_error", true: "success_false"}[falseSuccess], func(t *testing.T) {
			owner := localRequestTargetCDP(t, func(request cdpMessage) (any, *cdpError) {
				switch request.Method {
				case "Target.closeTarget":
					if falseSuccess {
						return map[string]any{"success": false}, nil
					}
					return nil, &cdpError{Code: -32000, Message: "fixture close refused"}
				case "Target.getTargets":
					return map[string]any{"targetInfos": []map[string]any{{"targetId": "owned-request"}}}, nil
				default:
					return map[string]any{}, nil
				}
			})
			page := localRequestTargetCDP(t, func(request cdpMessage) (any, *cdpError) {
				if request.Method != "Page.stopLoading" {
					t.Errorf("unexpected request-page cleanup: %s", request.Method)
				}
				// Successful stopLoading previously left the target alive.
				return map[string]any{}, nil
			})
			unrelated := localRequestTargetCDP(t, func(cdpMessage) (any, *cdpError) {
				return map[string]any{"targetInfos": []map[string]any{}}, nil
			})
			err := owner.closeRequestPage(page, "owned-request")
			if err == nil || !owner.closed.Load() || !page.closed.Load() || unrelated.closed.Load() {
				t.Fatalf("lost request target escaped cleanup or affected another session: err=%v owner=%v page=%v other=%v", err, owner.closed.Load(), page.closed.Load(), unrelated.closed.Load())
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err = unrelated.call(ctx, "Target.getTargets", nil); err != nil {
				t.Fatal("unrelated browser session is no longer usable:", err)
			}
		})
	}
}

func TestRequestTargetAlreadyGoneKeepsItsSession(t *testing.T) {
	owner := localRequestTargetCDP(t, func(request cdpMessage) (any, *cdpError) {
		if request.Method == "Target.closeTarget" {
			return nil, &cdpError{Code: -32000, Message: "No target with given id found"}
		}
		return map[string]any{"targetInfos": []map[string]any{}}, nil
	})
	page := localRequestTargetCDP(t, func(cdpMessage) (any, *cdpError) {
		t.Error("an already closed target must not be navigated or stopped")
		return map[string]any{}, nil
	})
	if err := owner.closeRequestPage(page, "already-closed"); err != nil || owner.closed.Load() || !page.closed.Load() {
		t.Fatalf("already-gone target destroyed its owner: err=%v owner=%v page=%v", err, owner.closed.Load(), page.closed.Load())
	}
}

func TestRequestTargetCanceledBeforeCreationKeepsItsSession(t *testing.T) {
	owner := localRequestTargetCDP(t, func(cdpMessage) (any, *cdpError) {
		t.Error("an already-canceled request must not create a browser target")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, targetID, err := owner.newPage(ctx)
	if !errors.Is(err, context.Canceled) || page != nil || targetID != "" || owner.closed.Load() {
		t.Fatalf("pre-canceled creation changed its session: err=%v closed=%v", err, owner.closed.Load())
	}
}

func TestRequestTargetCanceledCreationTerminatesOnlyItsOwnedSession(t *testing.T) {
	created := make(chan struct{})
	reply := make(chan struct{})
	defer close(reply)
	owner := localRequestTargetCDP(t, func(request cdpMessage) (any, *cdpError) {
		if request.Method != "Target.createTarget" {
			t.Errorf("unexpected request before creation acknowledgement: %s", request.Method)
			return nil, nil
		}
		close(created)
		<-reply // The request target exists before its identifier reaches Go.
		return map[string]any{"targetId": "late-request-target"}, nil
	})
	unrelated := localRequestTargetCDP(t, func(cdpMessage) (any, *cdpError) {
		return map[string]any{"targetInfos": []map[string]any{}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		page, targetID, err := owner.newPage(ctx)
		if page != nil || targetID != "" {
			t.Error("canceled target creation returned an active request page")
		}
		result <- err
	}()
	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture did not receive target creation")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || !owner.closed.Load() || unrelated.closed.Load() {
			t.Fatalf("unacknowledged target creation escaped owned cleanup: err=%v owner=%v other=%v", err, owner.closed.Load(), unrelated.closed.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled target creation did not finish cleanup")
	}
	checkCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := unrelated.call(checkCtx, "Target.getTargets", nil); err != nil {
		t.Fatal("unrelated browser session is no longer usable:", err)
	}
}
