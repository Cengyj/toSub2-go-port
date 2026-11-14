package authmaterial

import "testing"

func TestExportCannotHideSessionsBehindDuplicateContainers(t *testing.T) {
	for _, data := range []string{
		`{"extra":{"cookies":[{"name":"sid","value":"synthetic"}]},"extra":{}}`,
		`{"accounts":[{"extra":{"cookies":"synthetic"},"extra":{}}]}`,
		`{"extra":{"c\u006fokies":"synthetic"},"extra":{}}`,
		`{"extra":{},"extra":{"cookie_ref":"synthetic"}}`,
		`{"extra":{"oai-client-auth-session":{"workspace":"synthetic-login-context"}}}`,
		`{"extra":{"initial_login_progress":{"stage":"callback_ready","oauth":{"code":"synthetic","state":"synthetic"}}}}`,
		`{"extra":{"reauthentication_previous_checkpoint":{"stage":"web_password","web":{"session_id":"synthetic"}}}}`,
	} {
		if !ContainsLoginSessionJSON([]byte(data)) {
			t.Fatal("duplicate object concealed authentication material")
		}
	}
}

func TestExportPreservesOrdinaryCredentialAndBusinessSchema(t *testing.T) {
	for _, data := range []string{
		`{"accounts":[{"credentials":{"access_token":"synthetic","refresh_token":"synthetic"},"extra":{"session_id":"factor-binding","state":"active","checkpoint":"business-marker","fingerprint":{"session":"client-id"}}}]}`,
		`{"extra":{"precision":9007199254740993,"label":"session cookies are disabled"}}`,
	} {
		if ContainsLoginSessionJSON([]byte(data)) {
			t.Fatal("ordinary business fields rejected")
		}
	}
}

func TestEncodedSessionDocumentsCannotBecomeExportMetadata(t *testing.T) {
	for _, data := range []string{
		`{"extra":"{\"cookies\":[{\"value\":\"synthetic\"}]}"}`,
		`{"extra":"[{\"cookie_ref\":\"synthetic\"}]"}`,
	} {
		if !ContainsLoginSessionJSON([]byte(data)) {
			t.Fatal("encoded authentication container was accepted")
		}
	}
	if ContainsLoginSessionJSON([]byte(`{"extra":"{\"state\":\"active\",\"session_id\":\"business-binding\"}"}`)) {
		t.Fatal("encoded ordinary business metadata was rejected")
	}
}

func TestJSONValidationRejectsAmbiguousOrIncompleteDocuments(t *testing.T) {
	for _, data := range []string{`{"x":1,"x":2}`, `{"x":1,"\u0078":2}`, `{"x":1} {}`, `{"x":`, ``, `{"x":[1,]}`} {
		if ValidateUnambiguousJSON([]byte(data)) == nil {
			t.Fatal("invalid or ambiguous JSON was accepted")
		}
	}
	for _, data := range []string{`[]`, `null`, `{"x":9007199254740993}`, `{"x":{"x":1}}`} {
		if err := ValidateUnambiguousJSON([]byte(data)); err != nil {
			t.Fatal("valid JSON rejected", err)
		}
	}
}
