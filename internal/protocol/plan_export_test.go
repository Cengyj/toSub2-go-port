package protocol

import (
	"encoding/base64"
	"testing"
)

func TestOAuthExportIncludesOnlyRecognizedPlanMetadata(t *testing.T) {
	for _, tc := range []struct{ reported, want string }{{"plus", "plus"}, {"business", "team"}, {"private-unrecognized", ""}, {"", ""}} {
		id := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"fixture@example.test","https://api.openai.com/auth":{"chatgpt_plan_type":"`+tc.reported+`"}}`)) + ".synthetic"
		export, err := BuildSub2APIOAuthExport(TokenSet{IDToken: id, AccessToken: "fixture-access", RefreshToken: "fixture-refresh"}, "fixture-client", "fixture@example.test", "", 1, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := export.Accounts[0].Extra["plan_type"].(string)
		if got != tc.want {
			t.Errorf("export tier=%q, want %q", got, tc.want)
		}
	}
}
