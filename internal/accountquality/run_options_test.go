package accountquality

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/modelprobe"
)

func TestRunOptionsRequireBothExplicitUniqueValidFields(t *testing.T) {
	if selected, err := DecodeRunOptions(nil); err != nil || selected != nil {
		t.Fatal("omitted selection did not retain global behavior")
	}
	for _, raw := range []string{
		`null`, `{}`, `[]`, `{"model":"synthetic"}`, `{"reasoningEffort":"high"}`, `{"model":null,"reasoningEffort":"high"}`,
		`{"model":"synthetic","reasoningEffort":null}`, `{"model":"synthetic","reasoningEffort":""}`, `{"model":"synthetic","reasoningEffort":"unsupported"}`,
		`{"model":"synthetic","reasoningEffort":"high","extra":1}`, `{"model":"one","model":"two","reasoningEffort":"high"}`,
		`{"model":"synthetic","reasoningEffort":"high","\u0072easoningEffort":"low"}`, `{"model":" synthetic","reasoningEffort":"high"}`,
		`{"model":"synthetic\nheader","reasoningEffort":"high"}`, `{"model":"` + strings.Repeat("a", 129) + `","reasoningEffort":"high"}`,
	} {
		if _, err := DecodeRunOptions(json.RawMessage(raw)); err == nil {
			t.Fatalf("ambiguous/unsafe selection accepted: %s", raw)
		}
	}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		selected, err := DecodeRunOptions(json.RawMessage(`{"model":"synthetic-choice","reasoningEffort":"` + effort + `"}`))
		if err != nil || selected.Model != "synthetic-choice" || selected.ReasoningEffort != effort {
			t.Fatal("supported selection rejected", effort, err)
		}
	}
}

func TestAdvancedReasoningEffortsPersistAndReachProbe(t *testing.T) {
	for _, effort := range []string{"max", "ultra"} {
		t.Run(effort, func(t *testing.T) {
			s, f, root := newFixture(t)
			enable(t, s, func(settings *Settings) { settings.ReasoningEffort = effort })
			stored, err := ReadSettings(root)
			if err != nil || stored.ReasoningEffort != effort {
				t.Fatal("declared reasoning effort did not survive settings reload", err)
			}
			observed := make(chan modelprobe.Options, 2)
			f.mu.Lock()
			f.probe = func(_ context.Context, opts modelprobe.Options) (modelprobe.Result, error) {
				observed <- opts
				return resultFor(opts, 1), nil
			}
			f.mu.Unlock()
			if receipt, err := s.Refresh(t.Context(), []string{"a"}, false); err != nil || len(receipt.Accepted) != 1 {
				t.Fatal("configured advanced effort was not admitted", err)
			}
			if opts := <-observed; opts.ReasoningEffort != effort {
				t.Fatal("configured advanced effort was silently reduced")
			}
			idle(t, s)
			selected := &RunOptions{Model: "synthetic-selected", ReasoningEffort: effort}
			if receipt, err := s.RefreshWithOptions(t.Context(), []string{"a"}, false, selected); err != nil || len(receipt.Accepted) != 1 {
				t.Fatal("single-run advanced effort was not admitted", err)
			}
			if opts := <-observed; opts.Model != selected.Model || opts.ReasoningEffort != effort {
				t.Fatal("single-run advanced effort was silently reduced")
			}
			idle(t, s)
			after, err := s.Settings()
			if err != nil || after != stored {
				t.Fatal("single-run selection changed saved settings", err)
			}
		})
	}
}

func TestSingleRunSelectionExecutesAndPersistsItsOwnIdentityWithoutChangingPolicy(t *testing.T) {
	s, f, root := newFixture(t)
	before, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, directoryName, settingsName)
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	observed := make(chan modelprobe.Options, 1)
	f.mu.Lock()
	f.probe = func(_ context.Context, opts modelprobe.Options) (modelprobe.Result, error) {
		observed <- opts
		return resultFor(opts, 1), nil
	}
	f.mu.Unlock()
	selected := &RunOptions{Model: "synthetic-selected-model", ReasoningEffort: "low"}
	receipt, err := s.RefreshWithOptions(t.Context(), []string{"a"}, false, selected)
	if err != nil || len(receipt.Accepted) != 1 {
		t.Fatal("single-run probe was not admitted", err)
	}
	got := <-observed
	if got.Model != selected.Model || got.ReasoningEffort != selected.ReasoningEffort || got.MinimumScore != before.MinimumScore || got.TimeoutSeconds != before.TimeoutSeconds {
		t.Fatal("execution did not receive the selected model while retaining policy limits")
	}
	idle(t, s)
	snapshots, _ := s.Snapshots([]string{"a"})
	if snapshots["a"].LastResult == nil || snapshots["a"].LastResult.Model != selected.Model || !snapshots["a"].Stale || snapshots["a"].StaleReason != "options_changed" {
		t.Fatal("different-model observation was hidden or marked as a current global-policy result")
	}
	row := s.rows["a"]
	execution, _ := ApplyRunOptions(before, selected)
	if row.OptionsHash != optionsHash(execution) || row.OptionsHash == optionsHash(before) {
		t.Fatal("selected model did not participate in the persisted cache identity")
	}
	after, err := s.Settings()
	afterBytes, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || after != before || string(afterBytes) != string(beforeBytes) {
		t.Fatal("one observation changed global policy or revision")
	}
	calls := 0
	d, err := s.CheckForUpload(t.Context(), "a", upload("grant-a", func(_ context.Context, opts modelprobe.Options) (modelprobe.Result, error) {
		calls++
		if opts.Model != before.Model || opts.ReasoningEffort != before.ReasoningEffort {
			t.Fatal("upload inherited the one-off model choice")
		}
		return resultFor(opts, 1), nil
	}))
	if err != nil || !d.Allowed || d.Cached || calls != 1 {
		t.Fatalf("upload reused a different-model observation: %#v %v calls=%d", d, err, calls)
	}
}

func TestDifferentSelectionsNeitherMergeNorLetUploadTakeOverTheQueuedRequest(t *testing.T) {
	s, _, _ := newFixture(t)
	s.runGate <- struct{}{}
	defer func() { <-s.runGate }()
	selected := &RunOptions{Model: "synthetic-selected-model", ReasoningEffort: "low"}
	first, err := s.RefreshWithOptions(t.Context(), []string{"a"}, false, selected)
	if err != nil || len(first.Accepted) != 1 {
		t.Fatal("selected probe was not queued", err)
	}
	same, err := s.RefreshWithOptions(t.Context(), []string{"a"}, false, selected)
	if err != nil || len(same.Merged) != 1 || same.CheckID != first.CheckID {
		t.Fatal("identical selection did not merge")
	}
	different, err := s.Refresh(t.Context(), []string{"a"}, false)
	if err != nil || len(different.Merged) != 0 || len(different.Skipped) != 1 {
		t.Fatal("different selection was merged into the first request")
	}
	uploadCalls := 0
	d, err := s.CheckForUpload(t.Context(), "a", upload("grant-a", func(ctx context.Context, opts modelprobe.Options) (modelprobe.Result, error) {
		uploadCalls++
		return good(ctx, opts)
	}))
	if !errors.Is(err, ErrBusy) || d.Allowed || uploadCalls != 0 || s.flights["email-a"].settings.Model != selected.Model {
		t.Fatal("upload silently replaced a queued manual model choice", err)
	}
	if _, err := s.Cancel(t.Context(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
}
