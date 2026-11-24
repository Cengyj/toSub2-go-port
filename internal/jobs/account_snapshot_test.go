package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/store"
)

func TestPublicAccountsSelectsLatestAndDetachesPublicSnapshot(t *testing.T) {
	m := NewManager(t.Context(), 2, nil, nil)
	defer m.cancel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	newer := at.Add(time.Second)
	m.jobs["older"] = &Job{ID: "older", Email: " Owner@Example.Test ", CreatedAt: at, Logs: []string{"historic private tail"}}
	job := &Job{ID: "winner", Email: "owner@example.test", CreatedAt: newer, Status: StatusQueued, Input: map[string]any{"password": "synthetic-password", "totpSecret": "synthetic-seed"}, MailAPIURL: "https://mail.example.test/private", TOTPSetupSecret: "candidate", TOTPSetupURI: "otpauth://private", Prompt: "synthetic-password", Logs: []string{"tail"}, LogEntries: []LogEntry{{Message: "tail"}}, AutoRepairPendingAccountIDs: []int{3}, LastAuthRequirements: map[string]bool{"password": true}, StartedAt: &at}
	m.jobs[job.ID] = job
	m.jobs["tie-before"] = &Job{ID: "tie-before", Email: "other@example.test", CreatedAt: newer, Status: StatusQueued}
	m.pending = []*Job{m.jobs["tie-before"], job}
	got := m.PublicAccounts()
	if len(got) != 2 || got[0].Job.ID != "winner" || got[1].Job.ID != "tie-before" {
		t.Fatalf("wrong latest account ordering: %#v", got)
	}
	view := got[0]
	if !view.PasswordAvailable || !view.TOTPAvailable || !view.AutoEmailOTP || view.QueuePosition != 2 {
		t.Fatal("availability or queue projection changed")
	}
	if view.Job.Input != nil || view.Job.TOTPIntentReceipts != nil || view.Job.MailAPIURL != "" || view.Job.TOTPSetupSecret != "" || view.Job.TOTPSetupURI != "" || len(view.Job.Logs) != 0 || len(view.Job.LogEntries) != 0 {
		t.Fatal("account projection carried private input or history")
	}
	raw, err := json.Marshal(view)
	if err != nil || strings.Contains(string(raw), "synthetic-password") || strings.Contains(string(raw), "synthetic-seed") {
		t.Fatal("known secret leaked in public metadata", err)
	}
	view.Job.AutoRepairPendingAccountIDs[0] = 99
	view.Job.LastAuthRequirements["password"] = false
	*view.Job.StartedAt = time.Time{}
	if job.AutoRepairPendingAccountIDs[0] != 3 || !job.LastAuthRequirements["password"] || job.StartedAt.IsZero() {
		t.Fatal("public snapshot shares mutable state")
	}
	if len(m.List()) != 3 || len(job.LogEntries) != 1 || job.Input["password"] != "synthetic-password" {
		t.Fatal("list projection changed retained history or inputs")
	}
}

func TestPublicAccountsFilteredKeepsLatestGlobalCountsAndFilterContract(t *testing.T) {
	m := NewManager(t.Context(), 2, nil, nil)
	defer m.cancel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, job := range []*Job{
		{ID: "older", Email: " owner@example.test ", Status: StatusRunning, CreatedAt: at.Add(-time.Hour)},
		{ID: "tie-before", Email: "OWNER@example.test", Status: StatusSucceeded, ResultSaved: true, CreatedAt: at},
		{ID: "winner", Email: " Owner@Example.Test ", Status: StatusQueued, CreatedAt: at, Input: map[string]any{"password": "synthetic-filter-password"}},
		{ID: "other", Email: "other@example.test", Status: StatusRunning, CreatedAt: at.Add(time.Second)},
		{ID: "saved", Email: "saved@example.test", Status: StatusSucceeded, ResultSaved: true, CreatedAt: at.Add(-time.Second)},
	} {
		m.jobs[job.ID] = job
	}
	m.pending = []*Job{m.jobs["other"], m.jobs["winner"]}
	wantStats := AccountStatistics{Active: 1, Queued: 1, Completed: 1, Available: 1}
	all := m.PublicAccounts()
	for _, fixture := range []struct {
		name   string
		emails []string
		ids    []string
	}{
		{"nil", nil, []string{"other", "winner", "saved"}},
		{"empty", []string{}, []string{"other", "winner", "saved"}},
		{"blank", []string{" ", "\t"}, []string{"other", "winner", "saved"}},
		{"normalized-and-duplicate", []string{" OWNER@EXAMPLE.TEST ", "owner@example.test", ""}, []string{"winner"}},
		{"two", []string{"SAVED@example.test", "owner@example.test"}, []string{"winner", "saved"}},
		{"missing", []string{"missing@example.test"}, []string{}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			got := m.PublicAccountsFiltered(fixture.emails)
			if got.Total != 3 || got.Stats != wantStats || got.Accounts == nil || len(got.Accounts) != len(fixture.ids) {
				t.Fatalf("filtered snapshot changed global counts or empty-array contract: %#v", got)
			}
			for i, id := range fixture.ids {
				if got.Accounts[i].Job.ID != id {
					t.Fatal("filter changed latest-email selection or order")
				}
				var expected AccountSnapshot
				for _, item := range all {
					if item.Job.ID == id {
						expected = item
					}
				}
				if !reflect.DeepEqual(got.Accounts[i], expected) {
					t.Fatal("filtered projection changed availability, queue position, or public data")
				}
			}
		})
	}
}

func TestPublicAccountStatisticsKeepsSavedAuthorizationIndependentOfAction(t *testing.T) {
	m := NewManager(t.Context(), 1, nil, nil)
	defer m.cancel()
	for _, job := range []*Job{
		{ID: "failed-action", Email: "failed-action@example.test", Status: StatusSucceeded, OperationStatus: "failed", ResultSaved: true},
		{ID: "canceled-action", Email: "canceled-action@example.test", Status: StatusCanceled, OperationStatus: "canceled", ResultSaved: true},
		{ID: "blocked-result", Email: "blocked-result@example.test", Status: StatusSucceeded, ResultSaved: true, TOTPRecoveryPending: true},
		{ID: "missing-result", Email: "missing-result@example.test", Status: StatusSucceeded},
	} {
		m.jobs[job.ID] = job
	}
	for _, emails := range [][]string{nil, {"failed-action@example.test"}, {"absent@example.test"}} {
		got := m.PublicAccountsFiltered(emails)
		if got.Total != 4 || got.Stats != (AccountStatistics{Completed: 3, Available: 2}) {
			t.Fatalf("statistics confused the latest action with retained usable authorization: %#v", got.Stats)
		}
	}
}

func TestPublicAccountsConcurrentSnapshotsKeepCapturedSecretsAndMutableFields(t *testing.T) {
	m := NewManager(t.Context(), 2, nil, nil)
	defer m.cancel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	job := &Job{ID: "concurrent", Email: "concurrent@example.test", CreatedAt: at, Status: StatusQueued,
		AutoRepairPendingAccountIDs: []int{1}, AutoRepairQueuedRemotePlanTypes: []string{"team"},
		LastAuthRequirements: map[string]bool{"password": true}, StartedAt: &at,
		Input:  map[string]any{"headers": map[string]string{"X-Fixture": "snapshot-canary-initial"}},
		Prompt: "snapshot-canary-initial", AccountRequestObservations: []AccountRequestObservation{{Source: "usage", Status: "failed", ErrorCode: "upstream_error", FinishedAt: at, StartedAt: at}},
	}
	m.jobs[job.ID], m.pending = job, []*Job{job}
	start, stop := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			secret := fmt.Sprintf("snapshot-canary-%d", n)
			proxySecret := fmt.Sprintf("proxy-canary-%d/password", n)
			mailSecret := fmt.Sprintf("mail-canary-%d+token", n)
			m.mu.Lock()
			job.Input["headers"].(map[string]string)["X-Fixture"] = secret
			job.Input["proxyUrl"] = "https://" + url.UserPassword("fixture", proxySecret).String() + "@proxy.example.test"
			job.Input["mailApiUrl"] = "https://mail.example.test/?token=" + url.QueryEscape(mailSecret)
			job.Prompt = secret + " " + proxySecret + " " + mailSecret
			job.OperationError = job.Prompt
			job.AutoRepairPendingAccountIDs[0] = n
			job.LastAuthRequirements["password"] = n%2 == 0
			job.StartedAt = new(time.Time)
			*job.StartedAt = at.Add(time.Duration(n) * time.Second)
			job.LastOperationAt = *job.StartedAt
			job.AccountRequestObservations[0].StartedAt = job.LastOperationAt
			job.AccountRequestObservations[0].FinishedAt = job.LastOperationAt
			m.mu.Unlock()
		}
	}()
	close(start)
	for range 150 {
		got := m.PublicAccountsFiltered([]string{"CONCURRENT@example.test"})
		if len(got.Accounts) != 1 || got.Stats.Queued != 1 || got.Accounts[0].QueuePosition != 1 {
			t.Error("concurrent snapshot lost its queue state")
			continue
		}
		view := got.Accounts[0].Job
		if strings.Contains(view.Prompt, "canary-") || strings.Contains(view.OperationError, "canary-") || len(view.AccountRequestObservations) != 1 || view.AccountRequestObservations[0].Historical {
			t.Error("snapshot mixed a later secret or operation time into its captured public state")
		}
		view.AutoRepairPendingAccountIDs[0] = -1
		view.AutoRepairQueuedRemotePlanTypes[0] = "external-change"
		view.LastAuthRequirements["external"] = true
		*view.StartedAt = time.Time{}
		view.AccountRequestObservations[0].Source = "external-change"
	}
	close(stop)
	wg.Wait()
	if job.AutoRepairPendingAccountIDs[0] == -1 || job.AutoRepairQueuedRemotePlanTypes[0] != "team" || job.LastAuthRequirements["external"] || job.StartedAt.IsZero() || job.AccountRequestObservations[0].Source != "usage" {
		t.Fatal("returned snapshot shares mutable state with the live account")
	}
}

type accountSnapshotCredentialStore struct{ calls atomic.Int32 }

func (v *accountSnapshotCredentialStore) Load(string) (credentials.Credentials, error) {
	v.calls.Add(1)
	return credentials.Credentials{}, credentials.ErrUnavailable
}
func (v *accountSnapshotCredentialStore) Save(string, credentials.Credentials) error {
	v.calls.Add(1)
	return credentials.ErrUnavailable
}
func (v *accountSnapshotCredentialStore) Delete(string) error {
	v.calls.Add(1)
	return credentials.ErrUnavailable
}

func TestAccountSnapshotsNeverLoadCredentialsPersistOrAdmitWork(t *testing.T) {
	disk, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.Context(), 1, disk, nil)
	defer m.cancel()
	vault := &accountSnapshotCredentialStore{}
	m.SetCredentialStore(vault)
	var writes atomic.Int32
	m.metadataWriter = func(context.Context, string, any) error { writes.Add(1); return nil }
	m.SetRunner(func(context.Context, *Job, func(JobEvent)) error { t.Error("snapshot started work"); return nil })
	m.jobs["saved"] = &Job{ID: "saved", Email: "saved@example.test", Status: StatusSucceeded, ResultSaved: true}
	for _, emails := range [][]string{nil, {"saved@example.test"}, {"missing@example.test"}} {
		m.PublicAccountsFiltered(emails)
		m.QueryAccounts()
	}
	if vault.calls.Load() != 0 || writes.Load() != 0 || len(m.active) != 0 || len(m.pending) != 0 {
		t.Fatal("account-list projection performed credential I/O, persistence, or execution")
	}
}

func TestPublicAccountsDoesNotReadOrPersist(t *testing.T) {
	m := NewManager(context.Background(), 1, nil, nil)
	defer m.cancel()
	m.jobs["saved"] = &Job{ID: "saved", Email: "saved@example.test", Status: StatusSucceeded, ResultSaved: true, CreatedAt: time.Now()}
	m.metadataWriter = func(context.Context, string, any) error { t.Fatal("list wrote metadata"); return nil }
	if got := m.PublicAccounts(); len(got) != 1 || !got[0].Job.ResultSaved {
		t.Fatal("saved account disappeared")
	}
	if len(m.active) != 0 || len(m.pending) != 0 {
		t.Fatal("list started account work")
	}
}
