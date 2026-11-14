package console

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/credentials"
)

func TestRemovedReauthenticationOptionCannotQueueNewTOTPWorkflow(t *testing.T) {
	values := []struct {
		name  string
		value any
	}{
		{"true", true}, {"false", false}, {"null", nil},
		{"string", "false"}, {"object", map[string]any{}},
		{"array", []any{}}, {"number", 0},
	}
	for _, batch := range []bool{false, true} {
		for _, reset := range []bool{false, true} {
			for _, value := range values {
				name := map[bool]string{false: "single/", true: "batch/"}[batch] + map[bool]string{false: "setup/", true: "reset/"}[reset] + value.name
				t.Run(name, func(t *testing.T) {
					s, m := newMonitorTestServer(t)
					m.SetCredentialStore(credentials.NewMemoryStore())
					j := importTOTPActionFixture(t, m, "removed-reauthentication", reset)
					before, _ := m.Get(j.ID)
					body := map[string]any{"resetTotp": reset, "reauthenticate": value.value}
					path := "/api/jobs/" + j.ID + "/setup-2fa"
					if batch {
						path, body["ids"] = "/api/jobs/setup-2fa-batch", []string{j.ID}
					}
					w := frontendRequest(s, http.MethodPost, path, body, "test")
					if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不支持恢复远端会话") {
						t.Fatalf("removed option was not rejected: status=%d body=%s", w.Code, w.Body.String())
					}
					after, _ := m.Get(j.ID)
					if !reflect.DeepEqual(before, after) || m.QueuePosition(j.ID) != 0 {
						t.Fatal("removed recovery option changed or queued the account")
					}
				})
			}
		}
	}
}
