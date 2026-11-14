package console

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestRemovedSessionInputsCannotImportOrQueue(t *testing.T) {
	for _, key := range []string{"checkpointPath", "resumeCheckpoint", "checkpoint", "resume-checkpoint", "sessionOutputPath", "sessionPath", "webOnly", "noSub2APIExport", "outputMode", "cookie", "callback", "callbackUrl", "authorizationCode", "codeVerifier", "codeChallenge", "oauthState", "loginSession", "loginCheckpoint", "cookies", "cookie_ref", "cookie_state", "cookieRef", "cookieState", "reauthenticate", "Reauthenticate", "RESUME_CHECKPOINT", "CookieRef", "LOGIN_SESSION", "CALLBACK_URL"} {
		for _, value := range []any{nil, false, "synthetic-session"} {
			for _, route := range []string{"create", "create-input", "input", "retry", "relogin", "setup-2fa", "relogin-batch", "setup-2fa-batch"} {
				t.Run(key+"/"+route, func(t *testing.T) {
					s, m := newMonitorTestServer(t)
					m.SetCredentialStore(credentials.NewMemoryStore())
					j := importTOTPActionFixture(t, m, "removed-session", false)
					before, _ := m.Get(j.ID)
					body := map[string]any{key: value}
					path := "/api/jobs/" + j.ID + "/" + route
					switch route {
					case "create", "create-input":
						path = "/api/jobs"
						if route == "create-input" {
							body = map[string]any{"input": body}
						}
						body["email"] = "new-account@example.test"
					case "relogin-batch", "setup-2fa-batch":
						path = "/api/jobs/" + route
						body["ids"] = []string{j.ID}
					}
					w := frontendRequest(s, http.MethodPost, path, body, "test")
					if w.Code != http.StatusBadRequest {
						t.Fatalf("removed input accepted: route=%s status=%d", route, w.Code)
					}
					after, _ := m.Get(j.ID)
					if !reflect.DeepEqual(before, after) || m.QueuePosition(j.ID) != 0 || len(m.List()) != 1 {
						t.Fatal("removed session input changed persisted account or queue")
					}
				})
			}
		}
	}
}

func TestDuplicateAuthenticationRequestFieldsCannotQueue(t *testing.T) {
	for _, input := range []struct{ route, body string }{
		{"/api/jobs", `{"email":"new@example.test","operation":"recover-2fa","operation":"full"}`},
		{"/api/jobs", `{"email":"new@example.test","operation":"recover-2fa","Operation":"full"}`},
		{"/api/jobs", `{"email":"new@example.test","input":{"reauthenticate":true},"input":{}}`},
		{"/api/jobs", `{"email":"new@example.test","input":{"reauthenticate":true},"Input":{}}`},
		{"/api/jobs/batch", `{"jobs":[{"email":"new@example.test","input":{"cookie":"synthetic-obsolete-cookie"},"input":{}}]}`},
		{"/api/jobs/setup-2fa-batch", `{"ids":["duplicate-request"],"resetTotp":true,"ResetTotp":false}`},
		{"/api/jobs/setup-2fa-batch", `{"ids":["duplicate-request"],"resetTotp":true,"re\u017fetTotp":false}`},
		{"/api/jobs/duplicate-request/relogin", `{"reauthenticate":true,"reauthenticate":false}`},
		{"/api/jobs/duplicate-request/setup-2fa", `{"resetTotp":true,"resetTotp":false}`},
		{"/api/jobs/duplicate-request/input", `{"action":"email_otp","action":"password","value":"synthetic-input"}`},
	} {
		t.Run(input.route, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			m.SetCredentialStore(credentials.NewMemoryStore())
			job := importTOTPActionFixture(t, m, "duplicate-request", false)
			before, _ := m.Get(job.ID)
			request := httptest.NewRequest(http.MethodPost, input.route, strings.NewReader(input.body))
			request.Host = "127.0.0.1"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Console-Token", "test")
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, request)
			after, _ := m.Get(job.ID)
			if response.Code != http.StatusBadRequest || !reflect.DeepEqual(before, after) || m.QueuePosition(job.ID) != 0 || len(m.List()) != 1 {
				t.Fatalf("ambiguous request changed account or queue: status=%d", response.Code)
			}
		})
	}
}

func TestRemovedRemoteOperationsCannotCreateOrImportJobs(t *testing.T) {
	for _, operation := range []string{"recover-2fa", "inspect-2fa", "reauthenticate", " RECOVER-2FA ", "inspect_2fa", "Reauthenticate"} {
		for _, route := range []string{"create", "batch-root", "batch-nested"} {
			t.Run(operation+"/"+route, func(t *testing.T) {
				s, m := newMonitorTestServer(t)
				body := map[string]any{"email": "removed-operation@example.test", "operation": operation}
				path := "/api/jobs"
				if route != "create" {
					path = "/api/jobs/batch"
					if route == "batch-nested" {
						body = map[string]any{"jobs": []any{map[string]any{"email": "ordinary@example.test"}, body}}
					}
				}
				response := frontendRequest(s, http.MethodPost, path, body, "test")
				if response.Code != http.StatusBadRequest || len(m.List()) != 0 {
					t.Fatalf("removed operation created or queued a job: route=%s status=%d", route, response.Code)
				}
			})
		}
	}
}
