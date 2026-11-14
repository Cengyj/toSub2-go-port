package protocol

import (
	"strings"
	"testing"
)

func TestOAuthCallbackRejectsAmbiguousBinding(t *testing.T) {
	for _, suffix := range []string{
		"?state=expected&state=other&code=private-code",
		"?state=expected&code=private-code&code=other-code",
		"?state=expected&code=private-code&error=&error=access_denied",
		"?state=expected&code=private-code&extra=%zz",
		"?state=expected&code=private-code;ignored=value",
		"?state=expected&code=private-code#state=other",
	} {
		if _, err := ParseOAuthCallback(DefaultCodexRedirectURI+suffix, "expected"); err == nil {
			t.Error("ambiguous OAuth callback was accepted")
		} else if strings.Contains(err.Error(), "private-code") {
			t.Error("callback error exposed authorization code")
		}
	}
	flow := NewFlow(NewClient(nil, nil, ""), FlowOptions{})
	if _, err := flow.finishCallback("http://user:private-password@localhost:1455/auth/callback?state=expected&code=private-code", map[string]any{"state": "expected"}); err == nil {
		t.Fatal("callback with userinfo matched the registered redirect")
	}
	callback, err := ParseOAuthCallback(DefaultCodexRedirectURI+"?state=expected&code=private-code&provider_field=retained", "expected")
	if err != nil || callback.Code != "private-code" {
		t.Fatal("unambiguous callback with optional provider field was rejected", err)
	}
}
