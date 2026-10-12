package sub2api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMonitorExcludedPlansApplyBeforeEvaluationAndCoverDuplicateRecords(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "" {
			t.Error("exclusion scan must include healthy duplicate records")
		}
		_, _ = w.Write([]byte(`{"data":{"items":[
			{"id":1,"email":"duplicate@example.test","status":"error","group_ids":[7],"credentials":{"plan_type":"free"}},
			{"id":2,"email":"DUPLICATE@example.test","status":"active","group_ids":[8],"extra":{"plan_type":"Plus"}},
			{"id":3,"email":"local@example.test","status":"error","group_ids":[7]},
			{"id":4,"email":"unknown@example.test","status":"error","group_ids":[7]},
			{"id":5,"email":"allowed@example.test","status":"error","group_ids":[7],"plan_type":"free"},
			{"id":6,"email":"business@example.test","status":"error","group_ids":[7],"plan_type":"business"},
			{"id":7,"email":"outside@example.test","status":"error","group_ids":[8],"plan_type":"pro"},
			{"id":8,"email":"precedence@example.test","status":"error","group_ids":[7],"plan_type":"free"}
		],"pages":1}}`))
	}))
	defer remote.Close()
	evaluated, started := []string{}, []string{}
	s := NewMonitorService(New(Config{BaseURL: remote.URL, APIKey: "fixture"}), MonitorConfig{
		BaseURL: remote.URL, Enabled: true, GroupIDs: []int{7}, ExcludedPlanTypes: []string{"plus", "pro", "business", "unknown"},
	}, MonitorHooks{
		LocalPlanType: func(_ context.Context, email string) string {
			if email == "local@example.test" || email == "precedence@example.test" {
				return "pro"
			}
			return "unknown"
		},
		Evaluate: func(_ context.Context, email string, _ []RemoteAccount) (RepairDecision, error) {
			evaluated = append(evaluated, email)
			return RepairDecision{Action: ActionStart}, nil
		},
		StartRepair: func(_ context.Context, email string, _ []RemoteAccount) error {
			started = append(started, email)
			return nil
		},
	}, "")
	sum, err := s.Check(context.Background(), "manual")
	if err != nil || sum.Checked != 7 || sum.ExcludedPlan != 4 || sum.OutsideGroups != 1 || sum.Started != 2 {
		t.Fatalf("incorrect exclusion summary: %+v, %v", sum, err)
	}
	want := []string{"allowed@example.test", "precedence@example.test"}
	if !reflect.DeepEqual(evaluated, want) || !reflect.DeepEqual(started, want) {
		t.Fatalf("excluded account reached hooks: evaluated=%v started=%v", evaluated, started)
	}
	if len(s.State.Cooldowns) != 2 || s.State.Permanent["duplicate@example.test"] != "" {
		t.Fatal("excluded plans changed repair cooldown or permanent state")
	}
}

func TestMonitorPlanFilterDefaultsAndPreQueueGuard(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		excluded                  []string
		startError                error
		wantStarted, wantExcluded int
	}{
		{"default-allows-unknown", nil, nil, 1, 0},
		{"free-does-not-mean-unknown", []string{"free"}, nil, 1, 0},
		{"explicit-unknown", []string{"unknown"}, nil, 0, 1},
		{"changed-before-queue", []string{"pro"}, ErrPlanExcluded, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"data":{"items":[{"id":1,"email":"unknown@example.test","status":"error"}]}}`))
			}))
			defer remote.Close()
			s := NewMonitorService(New(Config{BaseURL: remote.URL, APIKey: "fixture"}), MonitorConfig{BaseURL: remote.URL, Enabled: true, ExcludedPlanTypes: tc.excluded}, MonitorHooks{
				Evaluate: func(context.Context, string, []RemoteAccount) (RepairDecision, error) {
					return RepairDecision{Action: ActionStart}, nil
				},
				StartRepair: func(context.Context, string, []RemoteAccount) error { return tc.startError },
			}, "")
			sum, err := s.Check(context.Background(), "timer")
			if err != nil || sum.Error != "" || sum.Started != tc.wantStarted || sum.ExcludedPlan != tc.wantExcluded {
				t.Fatalf("summary=%+v err=%v", sum, err)
			}
			if tc.wantExcluded > 0 && len(s.State.Cooldowns) != 0 {
				t.Fatal("exclusion incorrectly created a cooldown")
			}
		})
	}
}

func TestMonitorPlanConfigPersistsAndOwnsSlices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.json")
	values := []string{" PLUS ", "business", "team", "unknown", "bogus", "", "go", "education"}
	s := NewMonitorService(nil, MonitorConfig{ExcludedPlanTypes: values}, MonitorHooks{}, path)
	values[0] = "free"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	restored := NewMonitorService(nil, MonitorConfig{}, MonitorHooks{}, path)
	want := []string{"plus", "team", "unknown", "go", "edu"}
	if got := restored.Configuration().ExcludedPlanTypes; !reflect.DeepEqual(got, want) {
		t.Fatalf("restored=%v", got)
	}
	copy := restored.Configuration()
	copy.ExcludedPlanTypes[0] = "free"
	state := restored.PublicState()
	state.ExcludedPlanTypes[0] = "free"
	if !reflect.DeepEqual(restored.Configuration().ExcludedPlanTypes, want) {
		t.Fatal("caller mutated stored exclusions")
	}
	if err := restored.Configure(nil, MonitorConfig{}, MonitorHooks{}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(restored.PublicState())
	var public map[string]any
	_ = json.Unmarshal(data, &public)
	if got, ok := public["excludedPlanTypes"].([]any); !ok || len(got) != 0 {
		t.Fatal("cleared exclusions must be a JSON array")
	}
}
