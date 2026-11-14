package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func TestRefreshConflictingTokenIdentitiesRetainGrantWithoutRepeating(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source.json"), filepath.Join(root, "target.json")
	before := refreshRotationFixture(t, source)
	tokens := protocol.TokenSet{
		AccessToken:  metadataFixtureToken(`{"email":"first@example.test"}`),
		IDToken:      metadataFixtureToken(`{"email":"other@example.test"}`),
		RefreshToken: "synthetic-rotated-grant",
	}
	body, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		calls++
		return &transport.Response{StatusCode: 200, Body: body}, nil
	})
	for attempt := 0; attempt < 2; attempt++ {
		if err := refreshExistingExportAt(t.Context(), doer, "https://auth.example.test", "fallback-client", source, target, "first@example.test"); err == nil || !isLocalRefreshFailure(err) {
			t.Fatal("conflicting issued identities were accepted", err)
		}
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) || calls != 1 {
		t.Fatal("conflicting grant overwrote source or repeated remote rotation")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("conflicting grant produced an authorization output")
	}
	data, err := os.ReadFile(refreshRecoveryPath(target))
	if err != nil {
		t.Fatal("accepted rotated tokens were lost")
	}
	var receipt refreshGrantRecovery
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.Tokens != tokens {
		t.Fatal("retained grant did not preserve all accepted tokens")
	}
}

func TestRefreshRejectsEmbeddedLoginMaterialBeforeNetworkOrCopy(t *testing.T) {
	for _, field := range []string{"cookies", "loginCheckpoint", "callbackUrl", "codeVerifier", "webSession"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "source.json")
			target := filepath.Join(root, "target.json")
			before := refreshRotationFixture(t, source)
			raw, err := decodeRefreshDocument(before)
			if err != nil {
				t.Fatal(err)
			}
			raw["accounts"].([]any)[0].(map[string]any)["extra"].(map[string]any)[field] = map[string]any{"private": "synthetic-session-canary"}
			if err := protocol.WritePrivateJSON(source, raw); err != nil {
				t.Fatal(err)
			}
			original, _ := os.ReadFile(source)
			calls := 0
			doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				return &transport.Response{StatusCode: 200, Body: []byte(`{"access_token":"new"}`)}, nil
			})
			if err := refreshExistingExportAt(t.Context(), doer, "https://auth.example.test", "fallback-client", source, target, "first@example.test"); err == nil || !isLocalRefreshFailure(err) {
				t.Fatal("mixed old session refreshed", err)
			}
			if err := copyCLIPrivateJSON(source, target); err == nil {
				t.Fatal("mixed session exported")
			}
			after, _ := os.ReadFile(source)
			if calls != 0 || string(original) != string(after) {
				t.Fatal("rejection changed source or sent a request")
			}
			for _, p := range []string{target, refreshRecoveryPath(target)} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatal("rejected session copied to output", err)
				}
			}
		})
	}
}

func TestAcceptedRefreshWithSessionMaterialCannotWriteOrRepeatGrant(t *testing.T) {
	_, source, target := pendingRotationFixture(t)
	pending := refreshRecoveryPath(target)
	data, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["source"].(map[string]any)["cookies"] = []any{map[string]any{"name": "old", "value": "synthetic-cookie"}}
	if err := protocol.WritePrivateJSON(pending, record); err != nil {
		t.Fatal(err)
	}
	calls := 0
	doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) { calls++; return nil, nil })
	if err := refreshExistingExportAt(t.Context(), doer, "https://auth.example.test", "fallback-client", source, target, "first@example.test"); err == nil || !isLocalRefreshFailure(err) {
		t.Fatal("contaminated accepted result allowed fallback", err)
	}
	if calls != 0 {
		t.Fatal("accepted grant repeated over network")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("contaminated result written", err)
	}
}
