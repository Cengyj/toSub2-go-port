package protocol

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func protocolGolden(t *testing.T, name string) ([]byte, map[string]any) {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "go-port-golden", name))
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	if e = json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	return b, m
}
func jsonObject(t *testing.T, v any) map[string]any {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	if e = json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	return m
}

func TestGoldenSub2APIBuildShapeIncludesRefreshClientIdentity(t *testing.T) {
	_, expected := protocolGolden(t, "sub2api-import-oauth.json")
	claims := `{"email":"fixture@example.com","sub":"user_fixture","https://api.openai.com/auth":{"chatgpt_account_id":"acct_fixture","user_id":"user_fixture"}}`
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".synthetic"
	tokens := TokenSet{AccessToken: "fixture-access-token", RefreshToken: "fixture-refresh-token", IDToken: token}
	actual, e := BuildSub2APIOAuthExport(tokens, "app_fixture", "fixture@example.com", "", 10, 1, 1)
	if e != nil {
		t.Fatal(e)
	}
	// Client identity now travels inside credentials so either OAuth method
	// remains refreshable after Sub2API import; other legacy fields stay intact.
	expected["exported_at"] = actual.ExportedAt
	expected["accounts"].([]any)[0].(map[string]any)["credentials"].(map[string]any)["id_token"] = token
	expected["accounts"].([]any)[0].(map[string]any)["credentials"].(map[string]any)["client_id"] = "app_fixture"
	expected["accounts"].([]any)[0].(map[string]any)["credentials"].(map[string]any)["chatgpt_user_id"] = "user_fixture"
	if !reflect.DeepEqual(expected, jsonObject(t, actual)) {
		t.Fatal("generated OAuth JSON differs from the expected export shape")
	}
}

func TestGoldenPrivateOutputsPreserveUnknownFields(t *testing.T) {
	for _, name := range []string{"sub2api-import-oauth.json", "totp-setup-result.json", "password-add-result.json"} {
		t.Run(name, func(t *testing.T) {
			_, expected := protocolGolden(t, name)
			expected["future_extension"] = map[string]any{"nested": []any{"kept", true, float64(17)}}
			if name == "sub2api-import-oauth.json" {
				account := expected["accounts"].([]any)[0].(map[string]any)
				account["future_account"] = map[string]any{"route": "custom"}
				account["credentials"].(map[string]any)["future_credential"] = "preserve-opaque-value"
			}
			file := filepath.Join(t.TempDir(), name)
			if e := WritePrivateJSON(file, expected); e != nil {
				t.Fatal(e)
			}
			data, e := os.ReadFile(file)
			if e != nil {
				t.Fatal(e)
			}
			var got map[string]any
			if e = json.Unmarshal(data, &got); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(expected, got) {
				t.Fatal("private output lost an unknown field")
			}
			if st, e := os.Stat(file); e != nil {
				t.Fatal(e)
			} else if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
				t.Fatal("private output permissions are not 0600")
			}
		})
	}
}
