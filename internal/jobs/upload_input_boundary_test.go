package jobs

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestAuthenticationInputCannotRedirectAdmittedUpload(t *testing.T) {
	m, _, _, initial := resetManagerFixture(t)
	job, err := m.QueueOperation(initial.ID, "relogin", map[string]any{
		"uploadAfterRelogin": true, "uploadTarget": "cpa",
		"cpaConfig": map[string]any{"baseUrl": "https://confirmed.example.test", "adminApiKey": "confirmed-private-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
		emit(JobEvent{Stage: "phone_otp", Status: StatusWaitingInput})
		close(ready)
		<-ctx.Done()
		return ctx.Err()
	})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	for _, phase := range []string{"queued", "waiting"} {
		if phase == "waiting" {
			m.Start()
			select {
			case <-ready:
			case <-time.After(3 * time.Second):
				t.Fatal("runner did not reach input boundary")
			}
		}
		before, _ := m.InputCopy(job.ID)
		for _, key := range []string{"uploadTarget", "UPLOAD_TARGET", "cpaConfig", "CPA-CONFIG", "sub2apiConfig", "uploadAfterRelogin", "uploadAfterTotp", "reloginAfterTotp", "logoutAllDevicesAfterTotp", "resetTotp", "totpIntentId", "authorizationMethod", "operation"} {
			values := map[string]any{key: "changed-destination", "phone_otp": "112233"}
			if m.Input(job.ID, values) || m.InputForStage(job.ID, "phone_otp", values) {
				t.Fatalf("%s input rewrote workflow control %s", phase, key)
			}
			prepared := false
			if m.InputForStageWithPreparation(job.ID, "phone_otp", values, func() { prepared = true }) || prepared {
				t.Fatalf("%s control input changed SMS state before rejection: %s", phase, key)
			}
		}
		for _, values := range []map[string]any{
			{"uploadTarget": "sub2api"}, {"CPA_CONFIG": map[string]any{"baseUrl": "https://changed.example.test"}},
			{"cpaConfig": map[string]any{"baseUrl": "https://changed.example.test", "adminApiKey": "new-key"}},
			{"operation": "full"}, {"uploadAfterRelogin": false},
		} {
			if _, err := m.Update(job.ID, values); err == nil {
				t.Fatalf("%s account update rewrote a queued or running workflow", phase)
			}
			if _, _, err := m.SubmitUniqueEmail(Spec{Email: job.Email, Input: values}); err == nil {
				t.Fatalf("%s duplicate submission rewrote a queued or running workflow", phase)
			}
		}
		after, _ := m.InputCopy(job.ID)
		current, _ := m.Get(job.ID)
		if !reflect.DeepEqual(before, after) || current.UploadTarget != UploadTargetCPA || !current.UploadAfterRelogin || current.Operation != "relogin" {
			t.Fatal("rejected input changed the original upload intent")
		}
		if phase == "waiting" && current.Status != "phone_otp" {
			t.Fatal("rejected input closed the SMS prompt")
		}
	}
	// Ordinary authentication answers keep their existing compatibility.
	if !m.Input(job.ID, map[string]any{"password": " password with spaces "}) {
		t.Fatal("ordinary password input was rejected")
	}
	prepared := false
	if !m.InputForStageWithPreparation(job.ID, "phone_otp", map[string]any{"phone_otp": "112233"}, func() { prepared = true }) || !prepared {
		t.Fatal("ordinary SMS answer did not pass its preparation boundary")
	}
	values, _ := m.InputCopy(job.ID)
	if values["password"] != " password with spaces " || values["phone_otp"] != "112233" || values["uploadTarget"] != UploadTargetCPA || !reflect.DeepEqual(values["cpaConfig"], map[string]any{"baseUrl": "https://confirmed.example.test", "adminApiKey": "confirmed-private-key"}) {
		t.Fatal("authentication answer changed credentials or the confirmed destination")
	}
}
