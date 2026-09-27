package protocol

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestManagedCheckpointOutputCannotReopenStaleSession(t *testing.T) {
	for _, operation := range []string{"login", "setup-2fa", "add-password"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "checkpoint.json")
			cp := Checkpoint{Version: 1, Stage: "web_password", Email: "fixture@example.test", Cookies: []transport.Cookie{{Name: "stale-auth", Value: "private-stale-session", Domain: "chat.fixture", Path: "/"}}, Web: map[string]any{"deviceId": "old-device"}, OAuth: map[string]any{"pageURL": "https://auth.fixture/log-in/password", "loginMethod": "password"}}
			if err := WriteCheckpoint(path, cp); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			calls := 0
			doer := doerFunc(func(_ context.Context, request transport.Request) (*transport.Response, error) {
				calls++
				if request.URL != "https://chat.fixture/" {
					t.Error("managed fresh flow reused stale checkpoint", request.URL)
				}
				if request.Headers.Get("Cookie") != "" {
					t.Error("stale checkpoint cookie reached request")
				}
				return response(503, "fixture stops the fresh authentication request"), nil
			})
			flow := NewFlow(NewClient(doer, nil, ""), FlowOptions{CheckpointRestoreManaged: true, CheckpointPath: path, Email: cp.Email, ChatGPTBase: "https://chat.fixture", AuthBase: "https://auth.fixture"})
			var err error
			switch operation {
			case "login":
				_, err = flow.Run(context.Background())
			case "setup-2fa":
				_, err = flow.SetupTOTP(context.Background(), "")
			case "add-password":
				err = flow.AddPassword(context.Background(), "Fixture-Strong-Password-123!", "")
			}
			if err == nil || calls != 1 {
				t.Fatal("fresh flow failed to use its explicit restore decision", err, calls)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("unread checkpoint was modified")
			}
		})
	}
}

func TestManagedCheckpointStillAcceptsExplicitRestore(t *testing.T) {
	cp := Checkpoint{Version: 1, Stage: "email_verified", Email: "fixture@example.test", Cookies: []transport.Cookie{}, Web: map[string]any{"deviceId": "existing-device"}}
	flow := NewFlow(nil, FlowOptions{CheckpointRestoreManaged: true, Email: cp.Email})
	if err := flow.Restore(cp); err != nil {
		t.Fatal(err)
	}
	if err := flow.loadCheckpoint(); err != nil || flow.checkpoint == nil || flow.checkpoint.Stage != "email_verified" || flow.deviceID != "existing-device" {
		t.Fatal("explicit restore was discarded", err)
	}
}
