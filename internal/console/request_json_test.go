package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatefile"
)

func TestMailConfigurationRejectsAmbiguousOrNonObjectRequestsWithoutMutation(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `true`,
		`{"config":{"method":"POST","Method":"GET","url":"https://synthetic.invalid/mail"}}`,
		`{"config":{"method":"POST","url":"https://synthetic.invalid/mail","headers":{"X-Key":"one","x-key":"two"}}}`,
		`{"config":{"method":"POST","url":"https://synthetic.invalid/mail"},"Config":{"method":"GET"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			s := &Server{mail: mailRequestConfig{Method: http.MethodPost, URL: "https://retained.invalid/mail", Body: "retained", Headers: map[string]string{"X-Key": "synthetic-secret"}}}
			req := httptest.NewRequest(http.MethodPost, "/api/mail-request-config", strings.NewReader(body))
			response := httptest.NewRecorder()
			s.mailRequestConfig(response, req)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid request returned %d", response.Code)
			}
			if s.mail.Method != http.MethodPost || s.mail.URL != "https://retained.invalid/mail" || s.mail.Body != "retained" || s.mail.Headers["X-Key"] != "synthetic-secret" {
				t.Fatal("invalid request changed mail configuration")
			}
			if strings.Contains(response.Body.String(), "synthetic-secret") {
				t.Fatal("request error exposed private configuration")
			}
		})
	}
}

func TestAccountImportCaseInsensitiveExplicitClearsMatchTypedDecoding(t *testing.T) {
	s, m := newMonitorTestServer(t)
	m.SetCredentialStore(credentials.NewMemoryStore())
	seed := map[string]any{"email": "case-clear@example.invalid", "password": "Synthetic-password9!", "totpSecret": "JBSWY3DPEHPK3PXP", "mailApiUrl": "https://synthetic.invalid/mail", "mailRequestBody": "synthetic-body", "proxyUrl": "http://synthetic.invalid:8080"}
	created := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"jobs": []any{seed}}, "test")
	if created.Code != http.StatusCreated {
		t.Fatalf("fixture import returned %d", created.Code)
	}
	j := m.List()[0]
	omitted := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"jobs": []any{map[string]any{"EMAIL": j.Email}}}, "test")
	retained, _ := m.InputCopy(j.ID)
	if omitted.Code != http.StatusCreated || retained["password"] != seed["password"] || retained["mailRequestBody"] != seed["mailRequestBody"] {
		t.Fatal("omitted credentials changed existing account")
	}
	cleared := frontendRequest(s, http.MethodPost, "/api/jobs/batch", map[string]any{"jobs": []any{map[string]any{"EMAIL": j.Email, "PASSWORD": "", "TOTPSECRET": "", "MAILAPIURL": "", "MAILREQUESTBODY": "", "PROXYURL": ""}}}, "test")
	input, _ := m.InputCopy(j.ID)
	if cleared.Code != http.StatusCreated {
		t.Fatalf("unambiguous credential clears returned %d", cleared.Code)
	}
	for _, key := range []string{"password", "totpSecret", "mailApiUrl", "mailRequestBody", "proxyUrl"} {
		if input[key] != "" {
			t.Fatalf("explicit %s clear was treated as absent", key)
		}
	}
	_, err := m.LoadCredentials(j.ID)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("credential clear was not durably reflected in the private store")
	}
}

func TestRequestJSONRetainsSingleCaseInsensitiveFieldAndUnknownValues(t *testing.T) {
	var body struct {
		Config struct {
			Method string
			Extra  map[string]any
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"CONFIG":{"METHOD":"POST","extra":{"businessField":"retained"}}}`))
	response := httptest.NewRecorder()
	if !decodeJSON(response, req, &body) || body.Config.Method != "POST" || body.Config.Extra["businessField"] != "retained" {
		t.Fatal("unambiguous legacy case or business value was rejected")
	}
}

func TestUploadRequestPreservesOpaqueBusinessFieldsAndNumbers(t *testing.T) {
	for _, metadata := range []string{`"VendorSetting":1,"vendOrSetting":2`, `"sequence":9007199254740993,"ratio":1.0000000000000001`} {
		t.Run(metadata, func(t *testing.T) {
			var calls []string
			var uploaded []byte
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					fmt.Fprint(w, `{"data":{"items":[],"pages":1,"total":0}}`)
					return
				}
				uploaded, _ = io.ReadAll(r.Body)
				fmt.Fprint(w, `{"created":1}`)
			}))
			defer remote.Close()
			s, _ := newMonitorTestServer(t)
			payload := json.RawMessage(`{"accounts":[{"platform":"openai","type":"oauth","credentials":{"email":"vendor@example.invalid","access_token":"synthetic-access"},"extra":{` + metadata + `}}]}`)
			w := frontendRequest(s, http.MethodPost, "/api/sub2api/upload", map[string]any{"baseUrl": remote.URL, "adminApiKey": "synthetic-admin", "payload": payload}, "test")
			if w.Code != http.StatusOK || len(calls) != 2 {
				t.Fatalf("legal opaque upload rejected: status=%d calls=%v", w.Code, calls)
			}
			for _, field := range strings.Split(metadata, ",") {
				if !strings.Contains(string(uploaded), field) {
					t.Fatalf("upload changed business field %s", field)
				}
			}
		})
	}
}

func TestMonitorConfigurationUnknownReceiptNeverBecomesValidationFailure(t *testing.T) {
	w := httptest.NewRecorder()
	writeMonitorConfigurationError(w, fmt.Errorf("private path and key must stay hidden: %w", privatefile.ErrDurabilityUncertain))
	var body struct {
		OK        bool
		Outcome   string
		Uncertain bool
		Error     string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusServiceUnavailable || body.OK || body.Outcome != "unknown" || !body.Uncertain || strings.Contains(body.Error, "private path") {
		t.Fatal("ambiguous persistence was misclassified or exposed private details")
	}
}
