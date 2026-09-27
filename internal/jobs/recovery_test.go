package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestMetadataRoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	original := &Job{ID: "roundtrip", Email: "roundtrip@example.com", Operation: "relogin", Status: StatusFailed, CreatedAt: now, UpdatedAt: now, QueuedAt: now, StartedAt: &now, FinishedAt: &now, PasswordAddedAt: &now, Attempts: 4, Error: "failed", HasPassword: true, HasTOTPKey: true, ProxyConfigured: true, SecurityCheckRequired: true, AutoRepairPendingAccountIDs: []int{4, 9}, AutoRepairBackend: "backend", AutoRepairLastAttemptAt: &now, LastAuthAutomated: true, LastAuthRequirements: map[string]bool{"password": true}, LastAuthAutomatedAt: &now, Input: map[string]any{"password": "do-not-serialize"}}
	b, err := json.Marshal(jobMetadata(original))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "do-not-serialize") {
		t.Fatal("secret in metadata")
	}
	got, ok := decodeLegacyJob(b, "")
	if !ok {
		t.Fatal("decode failed")
	}
	want, _ := json.Marshal(jobMetadata(original))
	actual, _ := json.Marshal(jobMetadata(got))
	if string(want) != string(actual) {
		t.Fatalf("roundtrip\nwant %s\ngot %s", want, actual)
	}
}
func TestRestoreCheckpointIsNotCompletedAndNoPhantomJobs(t *testing.T) {
	st, _ := store.New(t.TempDir())
	for _, id := range []string{"checkpoint-only", "empty", "invalid"} {
		_ = os.MkdirAll(filepath.Join(st.Root, id), 0700)
	}
	_ = os.WriteFile(filepath.Join(st.Root, "checkpoint-only", "login-checkpoint.json"), []byte(`{"version":1,"email":"a@example.com"}`), 0600)
	_ = os.WriteFile(filepath.Join(st.Root, "invalid", "job-meta.json"), []byte(`{}`), 0600)
	m := NewManager(context.Background(), 20, st, nil)
	if e := m.Restore(context.Background()); e != nil {
		t.Fatal(e)
	}
	jobs := m.List()
	if len(jobs) != 1 || jobs[0].Status != "resume_available" || jobs[0].ResultSaved {
		t.Fatalf("restored %+v", jobs)
	}
}
func TestConsumeInputAliasesOnce(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	j, _ := m.Submit(Spec{Email: "a@example.com"})
	m.Input(j.ID, map[string]any{"email_otp": "123456", "value": "123456"})
	if _, ok := m.ConsumeInput(j.ID, "email_otp", "value"); !ok {
		t.Fatal("missing input")
	}
	if _, ok := m.ConsumeInput(j.ID, "email_otp", "value"); ok {
		t.Fatal("reused input alias")
	}
	m.Emit(JobEvent{JobID: j.ID, Stage: "email_otp", Status: StatusWaitingInput})
	got, _ := m.Get(j.ID)
	if got.Status != "email_otp" {
		t.Fatal(got.Status)
	}
}
func TestTwentyConcurrencyFIFOAndSnapshot(t *testing.T) {
	m := NewManager(context.Background(), 100, nil, nil)
	started := make(chan string, 25)
	release := make(chan struct{}, 25)
	m.SetRunner(func(ctx context.Context, j *Job, emit func(JobEvent)) error {
		started <- j.ID
		// Exercise snapshots concurrently with writes to the live manager.
		for i := 0; i < 100; i++ {
			_ = j.Status
			_ = j.Input["nested"].(map[string]any)["field"]
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	for i := 0; i < 25; i++ {
		_, e := m.Submit(Spec{ID: fmt.Sprintf("j%02d", i), Email: fmt.Sprintf("e%d@example.com", i), Input: map[string]any{"nested": map[string]any{"field": "original"}}})
		if e != nil {
			t.Fatal(e)
		}
	}
	m.Start()
	defer m.Shutdown(context.Background())
	for i := 0; i < 20; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("20 workers not started")
		}
	}
	select {
	case x := <-started:
		t.Fatalf("exceeded cap: %s", x)
	case <-time.After(20 * time.Millisecond):
	}
	for i := 0; i < 20; i++ {
		_, _ = m.Update(fmt.Sprintf("j%02d", i), map[string]any{"nested": map[string]any{"field": "changed"}})
	}
	for i := 20; i < 25; i++ {
		release <- struct{}{}
		select {
		case x := <-started:
			if x != fmt.Sprintf("j%02d", i) {
				t.Fatalf("FIFO: %s", x)
			}
		case <-time.After(time.Second):
			t.Fatal("queue stalled")
		}
	}
}
func TestShutdownRetainsQueuedWork(t *testing.T) {
	st, _ := store.New(t.TempDir())
	m := NewManager(context.Background(), 1, st, nil)
	started := make(chan struct{})
	m.SetRunner(func(ctx context.Context, j *Job, _ func(JobEvent)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	first, _ := m.Submit(Spec{ID: "first", Email: "first@example.com"})
	second, _ := m.Submit(Spec{ID: "second", Email: "second@example.com"})
	m.Start()
	<-started
	if e := m.Shutdown(context.Background()); e != nil {
		t.Fatal(e)
	}
	got, _ := m.Get(first.ID)
	if got.Status != "interrupted" {
		t.Fatal(got.Status)
	}
	got, _ = m.Get(second.ID)
	if got.Status != StatusQueued {
		t.Fatal(got.Status)
	}
	restored := NewManager(context.Background(), 1, st, nil)
	if e := restored.Restore(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(restored.pending) != 2 || restored.pending[0].ID != first.ID {
		t.Fatal("pending queue not recovered")
	}
}

func TestSuboperationCancelsCheckpointedWaitAndResumes(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	started := make(chan struct{}, 1)
	m.SetRunner(func(ctx context.Context, j *Job, emit func(JobEvent)) error {
		if j.Operation == "setup-2fa" {
			return nil
		}
		emit(JobEvent{Stage: "phone", Status: StatusWaitingInput})
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	j, _ := m.Submit(Spec{Email: "suboperation@example.com"})
	m.Start()
	defer m.Shutdown(context.Background())
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := m.SwitchOperation(ctx, j.ID, "setup-2fa", nil); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m, j.ID, "resume_available")
	next, err := m.Retry(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next.Operation != "resume" {
		t.Fatal("resume repeated suboperation")
	}
}

func TestNewCredentialsAndRotatedProxySurviveLaterOTP(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	v := credentials.NewMemoryStore()
	m.SetCredentialStore(v)
	j, e := m.Submit(Spec{Email: "delta@example.com", Input: map[string]any{"password": "Old-Password9!", "proxyUrl": "http://u-session-old:p@proxy.example:80"}})
	if e != nil {
		t.Fatal(e)
	}
	updated := credentials.Credentials{Password: "New-Password9!", ProxyURL: "http://u-session-new:p@proxy.example:80", PendingTOTPSecret: "PENDING-SEED"}
	if e := v.Save(j.Email, updated); e != nil {
		t.Fatal(e)
	}
	m.UpdateStoredCredentials(j.ID, updated)
	m.Input(j.ID, map[string]any{"email_otp": "123456", "value": "123456"})
	got, e := v.Load(j.Email)
	if e != nil {
		t.Fatal(e)
	}
	if got.Password != updated.Password || got.ProxyURL != updated.ProxyURL || got.PendingTOTPSecret != "PENDING-SEED" {
		t.Fatal("OTP overwrote protected credentials")
	}
	m.Update(j.ID, map[string]any{"proxyUrl": ""})
	got, _ = v.Load(j.Email)
	if got.ProxyURL != "" || got.Password != updated.Password {
		t.Fatal("explicit proxy clear failed or damaged password")
	}
}
