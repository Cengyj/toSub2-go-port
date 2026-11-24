package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatestate"
	"tosub2-go-port/internal/transport"
)

type passwordEvidenceProtector struct {
	credentials.StateProtector
	failBefore, failAfter bool
}

func (p *passwordEvidenceProtector) SealContext(ctx context.Context, identity string, plaintext, aad []byte) ([]byte, error) {
	var record PasswordAddRecovery
	if json.Unmarshal(plaintext, &record) == nil && (p.failBefore && record.State == "may_have_been_sent" || p.failAfter && record.State == "succeeded") {
		return nil, errors.New("synthetic protected write unavailable")
	}
	return p.StateProtector.SealContext(ctx, identity, plaintext, aad)
}

func TestPasswordAddJournalPreventsReplayAndRetainsLocalCompletion(t *testing.T) {
	for _, scenario := range []string{"success", "lost-response", "empty-success", "intent-preflight-failed", "success-journal-failed", "success-output-failed"} {
		t.Run(scenario, func(t *testing.T) {
			const candidate = "Synthetic-candidate-password9!"
			h := newRecoveryHarness(t)
			path := filepath.Join(t.TempDir(), "password-add-result.json")
			cipher, err := credentials.NewMemoryStateProtector()
			if err != nil {
				t.Fatal(err)
			}
			protector := &passwordEvidenceProtector{StateProtector: cipher, failBefore: scenario == "intent-preflight-failed", failAfter: scenario == "success-journal-failed"}
			if err := privatestate.Register(filepath.Dir(path), protector); err != nil {
				t.Fatal(err)
			}
			mutations := 0
			h.intercept = func(_ context.Context, request transport.Request) (*transport.Response, error, bool) {
				if strings.Contains(request.URL, "/api/auth/signin/openai?") && strings.Contains(request.URL, "post_login_add_password=true") {
					return fixtureJSON(map[string]any{"url": "https://auth.example/reset-password/new-password"}), nil, true
				}
				if pathOf(request.URL) != "/api/accounts/password/add" {
					return nil, nil, false
				}
				mutations++
				intent, err := readPasswordAddIntent(path, h.flow.Options.Email)
				if err != nil || intent.State != "may_have_been_sent" || intent.Password != candidate || intent.OperationID == "" || !request.SingleAttempt {
					t.Fatal("mutation dispatched before protected intent readback", err)
				}
				raw, err := os.ReadFile(PasswordAddIntentPath(path))
				if err != nil || bytes.Contains(raw, []byte(candidate)) || bytes.Contains(raw, []byte(h.flow.Options.Email)) {
					t.Fatal("intent was not protected")
				}
				switch scenario {
				case "lost-response":
					return nil, io.EOF, true
				case "empty-success":
					return response(http.StatusOK, `{}`), nil, true
				case "success-output-failed":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				return fixtureJSON(map[string]any{"continue_url": "https://chat.example/"}), nil, true
			}
			result, err := h.flow.AddPassword(t.Context(), candidate, path)
			wantRequests := 1
			if scenario == "intent-preflight-failed" {
				wantRequests = 0
			}
			if mutations != wantRequests || (err == nil) != (scenario == "success") || err == nil && result.Outcome != PasswordAdded {
				t.Fatal("wrong mutation outcome", mutations, result, err)
			}
			if scenario == "intent-preflight-failed" {
				return
			}
			prior, readErr := ReadPasswordAddRecovery(path, h.flow.Options.Email)
			if scenario == "lost-response" || scenario == "empty-success" {
				if prior != nil || !errors.Is(readErr, ErrPasswordAddUnconfirmed) {
					t.Fatal("unknown mutation produced local success", readErr)
				}
				fresh := NewFlow(doNotUseClient(), h.flow.Options)
				if _, retryErr := fresh.AddPassword(t.Context(), candidate, path); !errors.Is(retryErr, ErrPasswordAddUnconfirmed) {
					t.Fatal("unknown mutation allowed another login", retryErr)
				}
				return
			}
			if readErr != nil || prior == nil || prior.Password != candidate || prior.State != "succeeded" {
				t.Fatal("confirmed mutation lost local completion evidence", readErr)
			}
			if scenario == "success-output-failed" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			fresh := NewFlow(doNotUseClient(), h.flow.Options)
			result, err = fresh.AddPassword(t.Context(), candidate, path)
			if err != nil || result.Outcome != PasswordRecovered || mutations != 1 {
				t.Fatal("local completion replayed authentication or mutation", err)
			}
			var completion struct {
				OperationID string `json:"operation_id"`
				Password    string
			}
			raw, err := os.ReadFile(path)
			if err != nil || json.Unmarshal(raw, &completion) != nil || completion.OperationID != prior.OperationID || completion.Password != candidate {
				t.Fatal("local completion lost binding", err)
			}
		})
	}
}

func TestPasswordUnknownIntentDoesNotUseOlderResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password-add-result.json")
	intent := PasswordAddRecovery{Version: 1, OperationID: "new-operation", Email: "fixture@example.test", Password: "Synthetic-candidate-password9!", State: "may_have_been_sent"}
	if err := writePasswordAddIntent(path, intent); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateJSON(path, map[string]any{"version": 1, "email": intent.Email, "password": intent.Password, "operation_id": "old-operation", "added_at": nowRFC3339()}); err != nil {
		t.Fatal(err)
	}
	if recovered, err := ReadPasswordAddRecovery(path, intent.Email); recovered != nil || !errors.Is(err, ErrPasswordAddUnconfirmed) {
		t.Fatal("old result released current unknown mutation", err)
	}
}

func TestPasswordUnknownIntentRejectsAmbiguousCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		wantSuccess  bool
	}{
		{"duplicate", `"operation_id":"older-operation","operation_id":"new-operation"`, false},
		{"operation-alias", `"operation_id":"older-operation","OPERATION_ID":"new-operation"`, false},
		{"email-alias", `"operation_id":"new-operation","EMAIL":"different@example.test","Email":"fixture@example.test"`, false},
		{"unknown-extension", `"operation_id":"new-operation","extension":{"enabled":false,"attempts":0,"models":[]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "password-add-result.json")
			intent := PasswordAddRecovery{Version: 1, OperationID: "new-operation", Email: "fixture@example.test", Password: "Synthetic-candidate-password9!", State: "may_have_been_sent"}
			if err := writePasswordAddIntent(path, intent); err != nil {
				t.Fatal(err)
			}
			data := []byte(`{"version":1,"email":"fixture@example.test","password":"Synthetic-candidate-password9!","added_at":"2026-11-23T00:00:00Z",` + tc.fields + `}`)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			recovered, err := ReadPasswordAddRecovery(path, intent.Email)
			if tc.wantSuccess {
				if err != nil || recovered == nil || recovered.OperationID != intent.OperationID || recovered.State != "succeeded" {
					t.Fatal("unrelated legacy extension prevented exact operation recovery", err)
				}
			} else if recovered != nil || !errors.Is(err, ErrPasswordAddUnconfirmed) {
				t.Fatal("ambiguous completion released unknown mutation", err)
			}
		})
	}
}

func TestPasswordCleanupFailurePreservesSuccessReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password-add-result.json")
	receipt := []byte(`{"version":1,"operation_id":"synthetic-operation","email":"fixture@example.test","password":"Synthetic-candidate-password9!","added_at":"2026-11-23T00:00:00Z"}`)
	if err := os.WriteFile(path, receipt, 0600); err != nil {
		t.Fatal(err)
	}
	// A nonempty directory makes removal fail on every supported platform.
	// The successful receipt must survive when intent cleanup cannot finish.
	if err := os.Mkdir(PasswordAddIntentPath(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(PasswordAddIntentPath(path), "blocked"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemovePasswordAddEvidence(path); err == nil {
		t.Fatal("failed evidence removal was hidden")
	}
	if saved, err := os.ReadFile(path); err != nil || !bytes.Equal(saved, receipt) {
		t.Fatal("partial cleanup discarded the independent success receipt", err)
	}
}
