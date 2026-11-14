package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestSavedOAuthGrantRejectsAmbiguousJSON(t *testing.T) {
	grant := testIssuedGrant()
	valid, _ := json.Marshal(grant)
	for name, data := range map[string]string{
		"duplicate account": strings.Replace(string(valid), `"email":`, `"email":"another@example.test","email":`, 1),
		"duplicate token":   strings.Replace(string(valid), `"access_token":`, `"access_token":"other-access","access_token":`, 1),
		"case alias":        strings.Replace(string(valid), `"email":`, `"Email":"another@example.test","email":`, 1),
		"case token alias":  strings.Replace(string(valid), `"access_token":`, `"Access_Token":"other-access","access_token":`, 1),
		"null":              "null",
		"trailing":          string(valid) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "grant.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSavedOAuthGrant(path); err == nil {
				t.Fatal("ambiguous grant accepted")
			}
		})
	}
}

func TestSavedOAuthGrantRejectsOutputCollisionWithoutChangingGrant(t *testing.T) {
	for _, alias := range []string{"identical", "relative", "hard-link"} {
		t.Run(alias, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "grant.json")
			grant := testIssuedGrant()
			if err := WriteSavedOAuthGrant(path, grant); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			out := path
			if alias == "relative" {
				cwd, _ := os.Getwd()
				var err error
				out, err = filepath.Rel(cwd, path)
				if err != nil {
					t.Fatal(err)
				}
			} else if alias == "hard-link" {
				out = filepath.Join(filepath.Dir(path), "output.json")
				if err := os.Link(path, out); err != nil {
					t.Skip("hard links unavailable")
				}
			}
			if _, err := FinalizeSavedOAuthGrant(t.Context(), grant, FlowOptions{Email: grant.Email, GrantPath: path, OutputPath: out}); err == nil {
				t.Fatal("same grant/output accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("grant changed despite invalid output path", err)
			}
		})
	}
}

func TestSavedOAuthGrantOutputFailureOrCancellationPreservesIssuedResult(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "grant.json")
		grant := testIssuedGrant()
		if err := WriteSavedOAuthGrant(path, grant); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		out := filepath.Join(filepath.Dir(path), "output.json")
		ctx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
		} else if err := os.Mkdir(out, 0700); err != nil {
			t.Fatal(err)
		}
		_, err := FinalizeSavedOAuthGrant(ctx, grant, FlowOptions{Email: grant.Email, GrantPath: path, OutputPath: out})
		cancel()
		if err == nil || canceled && !errors.Is(err, context.Canceled) {
			t.Fatal("local failure was not returned", err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || string(before) != string(after) {
			t.Fatal("failed output lost the issued grant", readErr)
		}
	}
}

func TestSavedOAuthGrantRejectsConflictingTokenEmail(t *testing.T) {
	grant := testIssuedGrant()
	grant.Tokens.AccessToken = base64JWT(`{"email":"different@example.test","auth":{"chatgpt_account_id":"other-account"}}`)
	if _, err := FinalizeSavedOAuthGrant(t.Context(), grant, FlowOptions{Email: grant.Email}); err == nil {
		t.Fatal("access token changed selected account during export")
	}
}

func TestSavedOAuthGrantDoesNotReplaceOrRemoveDifferentIssuedResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grant.json")
	previous, current := testIssuedGrant(), testIssuedGrant()
	current.Tokens.RefreshToken = "newer-issued-refresh"
	if err := WriteSavedOAuthGrant(path, current); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	out := filepath.Join(filepath.Dir(path), "output.json")
	if _, err := FinalizeSavedOAuthGrant(t.Context(), previous, FlowOptions{Email: previous.Email, GrantPath: path, OutputPath: out}); err == nil {
		t.Fatal("different issued result was overwritten")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("newer grant was removed", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("stale result was exported before detecting changed grant")
	}
}

func TestOAuthOutputCollisionStopsBeforeLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grant.json")
	requests := 0
	f := NewFlow(NewClient(doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
		requests++
		return nil, errors.New("network must not start")
	}), nil, ""), FlowOptions{Email: "issued@example.test", OutputPath: path, GrantPath: path})
	if _, err := f.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "OAUTH_GRANT_OUTPUT_CONFLICT") || requests != 0 {
		t.Fatal("invalid output configuration started a login", err, requests)
	}
}

func TestJWTClaimsRejectDuplicateOrNonObjectIdentity(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{"email":"other","email":"expected"}`, `{"https://api.openai.com/profile":{"email":"other","email":"expected"}}`} {
		if _, err := DecodeJWTClaims(base64JWT(body)); err == nil {
			t.Fatal("ambiguous claims were accepted")
		}
	}
}

func TestOAuthTokenResponseIntegrityAndLateCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		complete   bool
		requestErr error
		wantOK     bool
	}{
		{"duplicate token", `{"access_token":"one","access_token":"two"}`, true, nil, false},
		{"success with error", `{"access_token":"issued","error":"invalid_grant"}`, true, nil, false},
		{"null", `null`, true, nil, false},
		{"partial", `{"access_token":"issued"}`, false, io.EOF, false},
		{"complete late error", `{"access_token":"issued","refresh_token":"rotated"}`, true, io.EOF, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			doer := doerFunc(func(context.Context, transport.Request) (*transport.Response, error) {
				requests++
				return &transport.Response{StatusCode: 200, Body: []byte(tc.body), BodyComplete: tc.complete}, tc.requestErr
			})
			tokens, err := RefreshOAuthToken(t.Context(), doer, "https://auth.example.test", DefaultCodexClientID, "previous")
			if (err == nil) != tc.wantOK || requests != 1 {
				t.Fatal("wrong grant classification or replay", err, requests)
			}
			if tc.wantOK && tokens.RefreshToken != "rotated" {
				t.Fatal("complete rotated grant lost")
			}
		})
	}
}
