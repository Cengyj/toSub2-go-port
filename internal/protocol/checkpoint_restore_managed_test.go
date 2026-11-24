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

			if err := os.WriteFile(path, []byte(`{"stage":"web_password","cookies":[{"name":"stale-auth","value":"private-stale-session"}],"oauth":{"codeVerifier":"old"}}`), 0600); err != nil {
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
			flow := NewFlow(NewClient(doer, nil, ""), FlowOptions{Email: "fixture@example.test", ChatGPTBase: "https://chat.fixture", AuthBase: "https://auth.fixture"})
			var err error
			switch operation {
			case "login":
				_, err = flow.Run(context.Background())
			case "setup-2fa":
				_, err = flow.SetupTOTP(context.Background(), "")
			case "add-password":
				_, err = flow.AddPassword(context.Background(), "Fixture-Strong-Password-123!", "")
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
