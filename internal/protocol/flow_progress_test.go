package protocol

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestFlowProgressKeepsCurrentOAuthBindingInMemory(t *testing.T) {
	f := NewFlow(nil, FlowOptions{ClientID: "current-client", RedirectURI: "http://localhost:1455/auth/callback"})
	f.web = map[string]any{"deviceId": "current-device", "emailVerified": true}
	oauth := map[string]any{
		"state": "current-state", "codeVerifier": "current-verifier",
		"callbackUrl": "http://localhost:1455/auth/callback?state=current-state&code=synthetic-code",
		"clientId":    "untrusted-client", "redirectUri": "https://unrelated.example.test/callback",
	}
	before := payloadCopy(oauth)
	f.recordProgress("callback_ready", oauth)
	if !reflect.DeepEqual(oauth, before) {
		t.Fatal("progress binding mutated its caller's OAuth map")
	}
	oauth["state"] = "later-state"
	progress := f.checkpoint
	if progress.Stage != "callback_ready" || progress.OAuth["clientId"] != f.Options.ClientID || progress.OAuth["redirectUri"] != f.Options.RedirectURI || progress.OAuth["state"] != "current-state" || progress.OAuth["codeVerifier"] != "current-verifier" || progress.OAuth["callbackUrl"] != before["callbackUrl"] {
		t.Fatal("in-memory transition lost the active OAuth transaction binding")
	}
	if !reflect.DeepEqual(progress.Web, f.web) {
		t.Fatal("progress lost the current operation's web state")
	}
	if _, err := json.Marshal(progress); !errors.Is(err, ErrLoginPersistenceRemoved) {
		t.Fatalf("in-memory login progress could be serialized: %v", err)
	}
	f.recordProgress("email_verified", nil)
	if f.checkpoint.Stage != "email_verified" || f.checkpoint.OAuth != nil || !reflect.DeepEqual(f.checkpoint.Web, f.web) {
		t.Fatal("web-only progress retained a previous OAuth transaction")
	}
}
