package accountquality

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tosub2-go-port/internal/modelprobe"
)

func writeLegacyCandyPolicy(t *testing.T, minimum int) (string, Settings, []byte) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, directoryName)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	legacy := Settings{SchemaVersion: 1, Revision: 17, Enabled: true, BeforeUpload: true, BlockDegraded: true, BlockFailed: false,
		Model: "synthetic-legacy-model", ReasoningEffort: "low", MinimumScore: minimum, TimeoutSeconds: 41, CooldownSeconds: 71, ResultTTLSeconds: 911}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, settingsName), data, 0600); err != nil {
		t.Fatal(err)
	}
	return root, legacy, data
}

func TestLegacySixQuestionThresholdIsReadWithoutWritingOrChangingPolicy(t *testing.T) {
	for minimum := 1; minimum <= 6; minimum++ {
		root, legacy, data := writeLegacyCandyPolicy(t, minimum)
		want := legacy
		want.MinimumScore = 1
		got, err := ReadSettings(root)
		if err != nil || got != want {
			t.Fatalf("legacy threshold %d changed another policy field: %#v %v", minimum, got, err)
		}
		service, err := New(root, fixtureProvider(&fixture{}))
		if err != nil {
			t.Fatal(err)
		}
		got, err = service.Settings()
		if err != nil || got != want {
			t.Fatalf("live policy did not use the same read-only normalization: %#v %v", got, err)
		}
		path := filepath.Join(root, directoryName, settingsName)
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, data) {
			t.Fatal("reading a legacy policy rewrote its document or revision", err)
		}
		saved, err := service.Update(t.Context(), got)
		want.Revision++
		if err != nil || saved != want {
			t.Fatalf("explicit save lost normalized policy or revision: %#v %v", saved, err)
		}
		var persisted Settings
		after, err = os.ReadFile(path)
		if err != nil || json.Unmarshal(after, &persisted) != nil || persisted != want {
			t.Fatal("explicit save did not persist the single-question threshold", err)
		}
	}
}

func TestCandyPolicyWritesRejectHistoricalThresholdsAndInvalidReadsStayClosed(t *testing.T) {
	for _, minimum := range []int{-1, 0, 2, 5, 6, 7} {
		root, legacy, data := writeLegacyCandyPolicy(t, minimum)
		if _, err := DecodeSettingsJSON(data); err == nil {
			t.Fatalf("API accepted obsolete threshold %d", minimum)
		}
		service, err := New(root, fixtureProvider(&fixture{}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = service.Update(t.Context(), legacy); err == nil {
			t.Fatalf("typed write accepted obsolete threshold %d", minimum)
		}
		if minimum < 1 || minimum > 6 {
			if _, err = ReadSettings(root); !errors.Is(err, ErrSettingsUnavailable) {
				t.Fatalf("invalid durable threshold %d became a default policy: %v", minimum, err)
			}
		}
	}
}

func TestLegacySixQuestionResultRemainsHistoryButCannotPassCandyGate(t *testing.T) {
	s, f, root := newFixture(t)
	c, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	old := modelprobe.Result{Model: c.Model, RuleVersion: "reasoning-six-v1", Verdict: "normal", Score: 6, Total: 6, CheckedAt: time.Now().UTC(), DurationMS: 10}
	legacyOptions := options(c)
	legacyOptions.MinimumScore = 5
	legacyHash, _ := json.Marshal([]any{"reasoning-six-v1", legacyOptions})
	row := storedRow{SchemaVersion: 1, AccountRevision: "r1", IdentityHash: digest("grant-a"), OptionsHash: digest(string(legacyHash)), Snapshot: Snapshot{ID: "a", Persisted: true, LastResult: &old, LastAttempt: Attempt{Status: "success"}}}
	if err = s.disk.WriteJSON(t.Context(), snapshotFile("a"), row); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err = New(root, fixtureProvider(f))
	if err != nil {
		t.Fatal(err)
	}
	s.Start(t.Context())
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	snapshots, _ := s.Snapshots([]string{"a"})
	if snapshots["a"].LastResult == nil || *snapshots["a"].LastResult != old || !snapshots["a"].Stale || snapshots["a"].StaleReason != "rule_changed" {
		t.Fatal("legacy 6/6 history was discarded or presented as a current candy result")
	}
	calls := 0
	d, err := s.CheckForUpload(t.Context(), "a", upload("grant-a", func(context.Context, modelprobe.Options) (modelprobe.Result, error) {
		calls++
		return modelprobe.Result{}, &modelprobe.Error{Code: "network_error"}
	}))
	// A failed new candy request must not fall back to the old six-task pass.
	if err == nil || d.Allowed || d.Cached || d.Result != nil || calls != 1 {
		t.Fatalf("legacy score bypassed candy gate: %#v %v, calls=%d", d, err, calls)
	}
}
