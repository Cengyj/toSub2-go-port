package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestPasswordAddRecoversOnlyFreshlyVerifiedMatchingCandidate(t *testing.T) {
	const pending = "Pending-Password9!"
	for _, scenario := range []string{
		"confirmed-pending", "expired-session-confirmed-pending",
		"cookie-only", "earlier-password-verification", "different-accepted-password",
		"rejected-pending", "login-continuation-failed", "verified-storage-failed",
	} {
		t.Run(scenario, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.flow.Options.Password = pending
			h.flow.Options.Input = func(context.Context, string, string) (string, error) { return "", context.Canceled }
			if scenario == "different-accepted-password" {
				h.flow.Options.Password = "Existing-Password9!"
			}
			if scenario == "cookie-only" || scenario == "earlier-password-verification" || scenario == "expired-session-confirmed-pending" {
				h.checkpoint(t, "email_verified", nil)
			}
			if scenario == "earlier-password-verification" {
				h.flow.verifiedPassword = pending
			}
			storageErr := errors.New("fixture protected storage unavailable")
			verified, preflights, mutations, recovered := 0, 0, 0, 0
			h.flow.Options.PasswordVerified = func(context.Context, string) error {
				verified++
				if scenario == "verified-storage-failed" {
					return storageErr
				}
				return nil
			}
			priorEvent := h.flow.Options.Event
			h.flow.Options.Event = func(stage, status, prompt string) {
				priorEvent(stage, status, prompt)
				if stage == "password_recovered" {
					recovered++
				}
				if strings.Contains(stage+status+prompt, pending) {
					t.Error("pending password leaked in event")
				}
			}
			h.intercept = func(_ context.Context, r transport.Request) (*transport.Response, error, bool) {
				u, _ := url.Parse(r.URL)
				switch u.Path {
				case "/authorize":
					return fixtureRedirect("https://auth.example/log-in/password"), nil, true
				case "/log-in/password":
					return response(http.StatusOK, "password page"), nil, true
				case "/api/accounts/password/verify":
					if scenario == "rejected-pending" {
						return response(http.StatusUnauthorized, "{\"error\":{\"code\":\"invalid_password\"}}"), nil, true
					}
					var input struct{ Password string }
					if err := json.Unmarshal(r.Body, &input); err != nil || input.Password != h.flow.Options.Password {
						t.Fatal("wrong candidate submitted for verification")
					}
					return fixtureJSON(map[string]any{"continue_url": "https://chat.example/web-callback"}), nil, true
				case "/web-callback":
					if scenario == "login-continuation-failed" {
						return nil, context.Canceled, true
					}
				case "/api/auth/signin/openai":
					if u.Query().Get("post_login_add_password") == "true" {
						preflights++
						target := "https://auth.example/reset-password/new-password"
						if scenario == "expired-session-confirmed-pending" && preflights == 1 {
							target = "https://auth.example/log-in"
						}
						return fixtureJSON(map[string]any{"url": target}), nil, true
					}
				case "/api/accounts/password/add":
					mutations++
					return fixtureJSON(map[string]any{"continue_url": "https://chat.example/"}), nil, true
				}
				return nil, nil, false
			}
			path := filepath.Join(t.TempDir(), "password-add-result.json")
			err := h.flow.AddPassword(context.Background(), pending, path)
			wantSuccess := scenario != "rejected-pending" && scenario != "login-continuation-failed" && scenario != "verified-storage-failed"
			if (err == nil) != wantSuccess {
				t.Fatalf("success mismatch: %v", err)
			}
			if scenario == "verified-storage-failed" && !errors.Is(err, storageErr) {
				t.Fatal("credential failure was not propagated")
			}
			wantMutation := 0
			if scenario == "cookie-only" || scenario == "earlier-password-verification" || scenario == "different-accepted-password" {
				wantMutation = 1
			}
			if mutations != wantMutation {
				t.Fatalf("password additions=%d want=%d", mutations, wantMutation)
			}
			wantRecovery := 0
			if scenario == "confirmed-pending" || scenario == "expired-session-confirmed-pending" {
				wantRecovery = 1
			}
			if recovered != wantRecovery {
				t.Fatalf("recoveries=%d want=%d", recovered, wantRecovery)
			}
			if wantRecovery > 0 && verified != 1 {
				t.Fatal("recovery without fresh server verification")
			}
			if h.flow.verifiedPassword != "" {
				t.Fatal("verified candidate survived operation")
			}
			data, readErr := os.ReadFile(path)
			if !wantSuccess {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatal("failed authentication generated success result")
				}
				return
			}
			var result struct {
				Version         int
				Email, Password string
				AddedAt         string `json:"added_at"`
			}
			if readErr != nil || json.Unmarshal(data, &result) != nil || result.Version != 1 || result.Email != h.flow.Options.Email || result.Password != pending || result.AddedAt == "" {
				t.Fatal("private result is not legacy compatible")
			}
			checkpoint, readErr := ReadCheckpoint(h.flow.Options.CheckpointPath)
			if readErr != nil || checkpoint.Stage != "email_verified" || checkpoint.OAuth != nil {
				t.Fatal("recovered checkpoint retained incomplete operation")
			}
			b, _ := json.Marshal(checkpoint)
			if strings.Contains(string(b), pending) {
				t.Fatal("pending password leaked to checkpoint")
			}
		})
	}
}
