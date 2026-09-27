package credentials

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyTruncatedKeychainRecoversOnlyCompleteFields(t *testing.T) {
	password, seed := "test-password", "JBSWY3DPEHPK3PXP"
	raw := `{"version":2,"password":"` + password + `","totpSecret":"` + seed + `","proxyUrl":"` + strings.Repeat("socks5h://user:secret@proxy.example:5000", 5) + `"}`
	raw = raw[:128]
	m := &memoryKeychain{values: map[string]string{macKeychainService: raw}}
	s := testMacStore(t, m)
	got, err := s.Load("test@example.com")
	if err != nil || got.Password != password || got.TOTPSecret != seed || got.ProxyURL != "" {
		t.Fatal("complete credential fields not recovered; incomplete proxy must be empty")
	}
	got.ProxyURL = "http://new:secret@proxy.example:8080"
	if err = s.Save("test@example.com", got); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load("test@example.com")
	if err != nil || got.Password != password || got.TOTPSecret != seed || got.ProxyURL == "" {
		t.Fatal("recovered credentials not migrated to encrypted envelope")
	}
	var plain Credentials
	if json.Unmarshal([]byte(m.values[macKeychainService]), &plain) == nil {
		t.Fatal("migration kept direct JSON keychain payload")
	}
}

func TestTruncatedCredentialRecoveryRejectsMalformedOrEncryptedData(t *testing.T) {
	for _, raw := range []string{
		`{"password":"incomplete`,
		`{"version":2,"password":"","proxyUrl":"incomplete`,
		`{"password":"complete",BROKEN`,
		`{"password":"complete","totpSecret":123}`,
		`{"password":"complete","password":"partial`,
		`{"version":99,"password":"complete","proxyUrl":"partial`,
		`{"version":1,"iv":"partial`,
		`{"password":"complete","ciphertext":"partial`,
		`dHJ1bmNhdGVkLWtleQ`,
		`{"password":"complete"} trailing`,
	} {
		if _, ok := recoverTruncatedLegacyCredentials(raw); ok {
			t.Fatal("malformed, incomplete or encrypted credential accepted")
		}
	}
	got, ok := recoverTruncatedLegacyCredentials(`{"password":"完整密码","totpSecret":"JBSWY3DPEHPK3PXP","proxyUrl":"https://proxy.example"`)
	if !ok || got.Password != "完整密码" || got.TOTPSecret == "" || got.ProxyURL != "https://proxy.example" {
		t.Fatal("complete UTF-8 values lost when only object end was truncated")
	}
}
