package jobs

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"tosub2-go-port/internal/store"
)

func TestProfileCompletionEventContainsOnlyDocumentedState(t *testing.T) {
	for _, outcome := range []string{"required", "submitting", "not_sent", "rejected", "unknown", "succeeded"} {
		event := JobEvent{Data: map[string]any{"profileCompletion": map[string]any{"trigger": "about_you", "source": "generated_adult", "outcome": outcome, "name": "Synthetic-private-name", "birthday": "1994-01-02"}}}
		out := SanitizeJobEvent(event)
		raw, _ := json.Marshal(out)
		profile, ok := out.Data["profileCompletion"].(map[string]any)
		if !ok || len(profile) != 3 || profile["outcome"] != outcome || strings.Contains(string(raw), "Synthetic-private-name") || strings.Contains(string(raw), "1994-01-02") {
			t.Fatal("profile status was lost or private values escaped")
		}
	}
	for _, profile := range []map[string]any{
		{"trigger": "generic_error", "source": "generated_adult", "outcome": "required"},
		{"trigger": "about_you", "source": "private-user-supplied-value", "outcome": "required"},
		{"trigger": "about_you", "source": "provided", "outcome": "private-upstream-response"},
	} {
		out := SanitizeJobEvent(JobEvent{Data: map[string]any{"profileCompletion": profile}})
		if _, exists := out.Data["profileCompletion"]; exists {
			t.Fatal("undocumented profile state escaped event boundary")
		}
	}
}

func TestCanceledRunnerRetainsOnlyFinalProfileEvidence(t *testing.T) {
	for _, outcome := range []string{"not_sent", "rejected", "unknown", "succeeded"} {
		t.Run(outcome, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(t.Context(), 1, st, nil)
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			ready, observed, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			done := func() { once.Do(func() { close(finish) }) }
			t.Cleanup(done)
			var late func(JobEvent)
			var runs atomic.Int32
			const secret = "synthetic-profile-private-password"
			observation := func(result string) JobEvent {
				return JobEvent{Stage: "profile_" + result, Status: StatusRunning, Prompt: "资料提交观察 " + secret, Data: map[string]any{
					"profileCompletion": map[string]any{"trigger": "about_you", "source": "generated_adult", "outcome": result, "birthday": "1994-01-02"},
					"mailStatus":        "error", "phoneRetryRequired": true, "providerBody": secret,
				}}
			}
			m.SetRunner(func(ctx context.Context, _ *Job, emit func(JobEvent)) error {
				runs.Add(1)
				late = emit
				emit(JobEvent{Stage: "profile_submitting", Status: StatusRunning, Prompt: "正在提交资料"})
				close(ready)
				<-ctx.Done()
				emit(observation("required"))
				emit(observation("submitting"))
				emit(observation("not-a-documented-outcome"))
				emit(observation(outcome))
				emit(JobEvent{Stage: "password", Status: "awaiting_input", Prompt: "迟到的输入请求"})
				close(observed)
				<-finish
				return ctx.Err()
			})
			job, err := m.Submit(Spec{Email: "profile-cancel@example.invalid", Input: map[string]any{"password": secret}})
			if err != nil {
				t.Fatal(err)
			}
			m.Start()
			waitDeferredSignal(t, ready, "profile submission")
			before, _ := m.Get(job.ID)
			events, unsubscribe := m.Subscribe(8)
			defer unsubscribe()
			if !m.Cancel(job.ID) {
				t.Fatal("running profile cancellation was rejected")
			}
			waitDeferredSignal(t, observed, "profile outcome observation")
			current, _ := m.Get(job.ID)
			if current.Status != before.Status || current.Stage != before.Stage || current.Prompt != before.Prompt || current.OperationStatus != before.OperationStatus || current.MailStatus != "" || current.PhoneRetryRequired || current.FinishedAt != nil {
				t.Fatal("canceled profile observation changed business or input state before cleanup")
			}
			if len(current.LogEntries) != len(before.LogEntries)+1 || len(events) != 1 {
				t.Fatalf("cancellation lost final profile evidence or admitted nonterminal events: added logs=%d, published=%d", len(current.LogEntries)-len(before.LogEntries), len(events))
			}
			last := current.LogEntries[len(current.LogEntries)-1]
			wantLevel := "warning"
			if outcome == "succeeded" {
				wantLevel = "info"
			}
			if last.Level != wantLevel || last.Status != "" || !last.OperationAt.Equal(before.LastOperationAt) || strings.Contains(last.Message, secret) {
				t.Fatal("final profile observation lost severity, operation binding or redaction")
			}
			event := <-events
			raw, _ := json.Marshal(event)
			profile, ok := event.Data["profileCompletion"].(map[string]any)
			if !ok || profile["outcome"] != outcome || len(profile) != 3 || len(event.Data) != 1 || event.Status != "" || event.JobID != job.ID || strings.Contains(string(raw), secret) || strings.Contains(string(raw), "1994-01-02") {
				t.Fatal("profile event exposed private or state-changing fields")
			}
			done()
			ended := waitDeferredFinished(t, m, job.ID)
			if ended.OperationStatus != "canceled" || ended.OperationError != "" || runs.Load() != 1 {
				t.Fatal("retaining profile evidence changed cancellation or replayed the runner")
			}
			late(observation("unknown"))
			after, _ := m.Get(job.ID)
			if !reflect.DeepEqual(ended.LogEntries, after.LogEntries) || !ended.UpdatedAt.Equal(after.UpdatedAt) {
				t.Fatal("returned runner appended a stale profile observation")
			}
			if err := m.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored := NewManager(t.Context(), 1, st, nil)
			t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
			if err := restored.Restore(t.Context()); err != nil {
				t.Fatal(err)
			}
			restarted, _ := restored.Get(job.ID)
			if restarted.OperationStatus != "canceled" || !reflect.DeepEqual(ended.LogEntries, restarted.LogEntries) || restored.QueuePosition(job.ID) != 0 {
				t.Fatal("restart lost profile evidence or resumed canceled work")
			}
		})
	}
}
