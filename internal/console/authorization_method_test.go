package console

import (
	"net/http"
	"reflect"
	"testing"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

func TestAuthorizationMethodRejectsInvalidInputBeforeAnyMutation(t *testing.T) {
	for _, route := range []string{"create", "batch", "retry", "regenerate", "relogin", "relogin-batch", "reauthorize-batch", "setup-2fa", "setup-2fa-batch"} {
		for _, invalid := range []any{nil, true, 1, "BPS", "unknown", map[string]any{"method": "bps"}} {
			t.Run(route+"/"+reflect.TypeOf(struct{ V any }{invalid}).String(), func(t *testing.T) {
				s, m := newMonitorTestServer(t)
				m.SetCredentialStore(credentials.NewMemoryStore())
				job := importTOTPActionFixture(t, m, "authorization-method", true)
				before, _ := m.Get(job.ID)
				body := map[string]any{"authorizationMethod": invalid, "proxyUrl": "http://new-proxy.example:8080"}
				path := "/api/jobs/" + job.ID + "/" + route
				switch route {
				case "create":
					path, body["email"] = "/api/jobs", "new@example.test"
				case "batch":
					path, body["emails"] = "/api/jobs/batch", []string{"new@example.test", "second@example.test"}
				case "relogin-batch", "reauthorize-batch", "setup-2fa-batch":
					path, body["ids"] = "/api/jobs/"+route, []string{job.ID}
				}
				if route == "setup-2fa" || route == "setup-2fa-batch" {
					body["reloginAfterTotp"] = true
				}
				response := frontendRequest(s, http.MethodPost, path, body, "test")
				after, _ := m.Get(job.ID)
				if response.Code != http.StatusBadRequest || len(m.List()) != 1 || !reflect.DeepEqual(before, after) {
					t.Fatalf("invalid authorization method mutated job: status=%d body=%s", response.Code, response.Body)
				}
			})
		}
	}
}

func TestAuthorizationMethodSupportedByEveryAuthorizationEntry(t *testing.T) {
	for _, route := range []string{"create", "retry", "regenerate", "relogin", "relogin-batch", "reauthorize-batch", "setup-2fa", "setup-2fa-batch"} {
		t.Run(route, func(t *testing.T) {
			s, m := newMonitorTestServer(t)
			m.SetCredentialStore(credentials.NewMemoryStore())
			job := importTOTPActionFixture(t, m, "authorization-method", true)
			if route == "regenerate" {
				m.UpdateResult(job.ID, true, "fixture result")
				m.Emit(jobs.JobEvent{JobID: job.ID, Status: jobs.StatusSucceeded})
			}
			body := map[string]any{"authorizationMethod": "bps"}
			path := "/api/jobs/" + job.ID + "/" + route
			switch route {
			case "create":
				path, body["email"] = "/api/jobs", "new@example.test"
			case "relogin-batch", "reauthorize-batch", "setup-2fa-batch":
				path, body["ids"] = "/api/jobs/"+route, []string{job.ID}
			}
			if route == "setup-2fa" || route == "setup-2fa-batch" {
				body["reloginAfterTotp"], body["resetTotp"] = true, true
			}
			response := frontendRequest(s, http.MethodPost, path, body, "test")
			if response.Code != http.StatusOK && response.Code != http.StatusCreated {
				t.Fatal(response.Code, response.Body.String())
			}
			current, _ := m.Get(job.ID)
			if route == "create" {
				current, _ = m.FindByEmail("new@example.test")
			}
			if current.AuthorizationMethod != "bps" || s.publicJob(current)["authorizationMethod"] != "bps" || s.publicSelectionJob(current)["authorizationMethod"] != "bps" {
				t.Fatal("authorization selection lost at scheduler or public boundary")
			}
			if route == "regenerate" && current.Operation != "relogin" {
				t.Fatal("switching authorization method attempted refresh with the old client")
			}
		})
	}
}

func TestSecurityOnlyActionsDoNotChangeAuthorizationMethod(t *testing.T) {
	for _, action := range []string{"setup-2fa", "add-password", "delete"} {
		if _, err := normalizeTOTPAction(action, map[string]any{"authorizationMethod": "bps"}); err == nil {
			t.Fatal("security-only operation silently changed authorization method", action)
		}
	}
}
