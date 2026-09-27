package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/transport"
)

func TestPKCES256(t *testing.T) {
	p, e := NewPKCE()
	if e != nil {
		t.Fatal(e)
	}
	if e = p.Validate(); e != nil {
		t.Fatal(e)
	}
	if VerifyPKCE(p.CodeVerifier, "bad") {
		t.Fatal("bad challenge accepted")
	}
}
func TestTOTPVector(t *testing.T) { // RFC 6238 SHA-1 vector (secret "12345678901234567890")
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	got, e := TOTPAtUnix(secret, 59)
	if e != nil {
		t.Fatal(e)
	}
	if got != "287082" {
		t.Fatalf("got %s", got)
	}
}
func TestMailboxOTPExtraction(t *testing.T) {
	raw := `{"messages":[{"id":"a","received_at":"2026-09-25T10:00:00Z","subject":"OpenAI verification code","body":"Your code is 123456"},{"id":"b","received_at":"2026-09-25T11:00:00Z","subject":"verification code","body":"Your code is 654321"}]}`
	got := ExtractMailboxOTPCandidates(raw)
	if len(got) < 2 || got[0].Code != "654321" {
		t.Fatalf("candidates=%#v", got)
	}
	filtered := FilterMailboxCandidatesByRequestTime(got, time.Date(2026, 9, 25, 10, 30, 0, 0, time.UTC).UnixMilli())
	if len(filtered) != 1 || filtered[0].Code != "654321" {
		t.Fatalf("filtered=%#v", filtered)
	}
}
func TestCheckpointAtomicAndCompatible(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "checkpoint.json")
	c := Checkpoint{Version: 1, Stage: "email_verified", Email: "a@example.com", Cookies: []transport.Cookie{{Name: "sid", Value: "v", Domain: "example.com", Path: "/"}}, Extra: map[string]any{"future": true}}
	if e := WriteCheckpoint(f, c); e != nil {
		t.Fatal(e)
	}
	private, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	if err = privatefile.Verify(private); err != nil {
		t.Fatal(err)
	}
	got, e := ReadCheckpoint(f)
	if e != nil {
		t.Fatal(e)
	}
	if got.Email != c.Email || got.Extra["future"] != true {
		t.Fatalf("%#v", got)
	}
	b, _ := os.ReadFile(f)
	if !strings.Contains(string(b), "email_verified") {
		t.Fatal("missing stage")
	}
}

func TestOAuthTokenExchangeAndSub2APIShape(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"sid":"acct","email":"a@example.com","sub":"user","https://api.openai.com/auth":{"user_id":"user"}}`))
	id := header + "." + claims + ".x"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "r", "id_token": id})
	}))
	defer s.Close()
	tok, err := ExchangeOAuthCode(context.Background(), transport.NewClient(nil), s.URL, "cid", "code", "verifier", "http://localhost/cb")
	if err != nil {
		t.Fatal(err)
	}
	ex, err := BuildSub2APIOAuthExport(tok, "cid", "", "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ex.Type != "sub2api-data" || len(ex.Accounts) != 1 || ex.Accounts[0].Credentials.Email != "a@example.com" {
		t.Fatalf("export=%#v", ex)
	}
}

func TestSub2APIExplicitAccountClaimOverridesSessionSID(t *testing.T) {
	jwt := func(payload string) string {
		return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".x"
	}
	for _, tc := range []struct{ id, access, want string }{
		{`{"sid":"session-id","https://api.openai.com/auth":{"chatgpt_account_id":"account-id"}}`, "opaque", "account-id"},
		{`{"sid":"session-id"}`, jwt(`{"https://api.openai.com/auth":{"chatgpt_account_id":"access-account-id"}}`), "access-account-id"},
		{`{"sid":"legacy-account"}`, "opaque", "legacy-account"},
	} {
		out, e := BuildSub2APIOAuthExport(TokenSet{AccessToken: tc.access, IDToken: jwt(tc.id), RefreshToken: "r"}, "client", "a@example.com", "", 10, 1, 1)
		if e != nil {
			t.Fatal(e)
		}
		a := out.Accounts[0]
		if a.Credentials.ChatGPTAccountID != tc.want || a.Extra["account_id"] != tc.want || a.Extra["chatgpt_account_id"] != tc.want {
			t.Fatalf("account claim mismatch: %#v", a)
		}
	}
}
