package console

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/grafana/sobek"
)

type monitorResultPresentation struct {
	Code, Label, Tone, Icon, Text string
	Spinning                      bool
}

func monitorPresentationCall(t *testing.T, vm *sobek.Runtime, name string, target any, args ...any) {
	t.Helper()
	fn, ok := sobek.AssertFunction(vm.Get("Presentation").ToObject(vm).Get(name))
	if !ok {
		t.Fatalf("missing %s export", name)
	}
	values := make([]sobek.Value, len(args))
	for i, arg := range args {
		values[i] = vm.ToValue(arg)
	}
	value, err := fn(sobek.Undefined(), values...)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value.Export())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}

func presentMonitorResult(t *testing.T, vm *sobek.Runtime, result any, lastError string) monitorResultPresentation {
	t.Helper()
	var got monitorResultPresentation
	monitorPresentationCall(t, vm, "describeMonitorResult", &got, result, lastError)
	return got
}

func TestMonitorResultPresentationDistinguishesSkippedQueuedAndUpdated(t *testing.T) {
	vm := presentationVM(t)
	cases := []struct {
		name, code, tone string
		result           any
	}{
		{"no previous result", "empty", "neutral", nil},
		{"no abnormal records", "clear", "neutral", map[string]any{"checked": 0}},
		{"checked without action", "no_action", "neutral", map[string]any{"checked": 1}},
		{"plan exclusion is not success", "excluded", "neutral", map[string]any{"checked": 1, "excludedPlan": 1}},
		{"busy without new repair", "deferred", "neutral", map[string]any{"checked": 1, "busy": 1}},
		{"cooldown without new repair", "deferred", "neutral", map[string]any{"checked": 1, "cooldown": 1}},
		{"outside selected groups", "skipped", "neutral", map[string]any{"checked": 1, "outsideGroups": 1}},
		{"repair queued", "queued", "info", map[string]any{"checked": 2, "started": 2}},
		{"queued with exclusion", "queued", "info", map[string]any{"checked": 3, "started": 2, "excludedPlan": 1}},
		{"queued and updated", "queued", "info", map[string]any{"checked": 2, "started": 2, "updated": 3}},
		{"updated records", "updated", "success", map[string]any{"checked": 3, "updated": 3}},
		{"previous authorization updated with no current abnormal records", "updated", "success", map[string]any{"checked": 0, "updated": 3}},
		{"updated with exclusion", "partial", "info", map[string]any{"checked": 1, "updated": 3, "excludedPlan": 1}},
		{"updated with busy records", "partial", "info", map[string]any{"checked": 1, "updated": 3, "busy": 1}},
		{"updated with cooldown", "partial", "info", map[string]any{"checked": 1, "updated": 3, "cooldown": 1}},
		{"updated with outside groups", "partial", "info", map[string]any{"checked": 1, "updated": 3, "outsideGroups": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := presentMonitorResult(t, vm, tc.result, "")
			if got.Code != tc.code || got.Tone != tc.tone || got.Spinning {
				t.Fatalf("unexpected result presentation: %+v", got)
			}
			if got.Label == "" || got.Icon == "" || got.Text == "" {
				t.Fatalf("incomplete result presentation: %+v", got)
			}
			if tc.code == "excluded" && (!strings.Contains(got.Label, "按套餐跳过") || !strings.Contains(got.Text, "未启动自动重登")) {
				t.Fatalf("plan exclusion was presented as a completed repair: %+v", got)
			}
			if tc.code == "queued" && (!strings.Contains(got.Text, "已排队自动重登") || strings.Contains(got.Label+got.Text, "修复成功")) {
				t.Fatalf("queued repair was presented as completed: %+v", got)
			}
		})
	}
}

func TestMonitorResultPresentationPrioritizesAttentionAndErrors(t *testing.T) {
	vm := presentationVM(t)
	for _, field := range []string{"blocked", "ineligible", "missingTask", "missingEmail"} {
		t.Run(field, func(t *testing.T) {
			result := map[string]any{"checked": 4, "started": 1, "updated": 2, "excludedPlan": 1, field: 1}
			got := presentMonitorResult(t, vm, result, "")
			if got.Code != "attention" || got.Tone != "warning" {
				t.Fatalf("%s was hidden by partial progress: %+v", field, got)
			}
			result["error"] = "synthetic result failure"
			got = presentMonitorResult(t, vm, result, "")
			if got.Code != "error" || got.Tone != "warning" || !strings.Contains(got.Text, "synthetic result failure") {
				t.Fatalf("result error was hidden by counts: %+v", got)
			}
		})
	}
	for _, result := range []any{nil, map[string]any{"checked": 0, "updated": 2, "error": "synthetic result failure"}} {
		got := presentMonitorResult(t, vm, result, "synthetic state failure")
		if got.Code != "error" || got.Tone != "warning" || !strings.Contains(got.Text, "synthetic state failure") || strings.Contains(got.Text, "synthetic result failure") {
			t.Fatalf("explicit state error did not take priority: %+v", got)
		}
	}
}

func TestMonitorResultPresentationRetainsAllOutcomeCounts(t *testing.T) {
	vm := presentationVM(t)
	result := map[string]any{
		"checked": 41, "started": 2, "updated": 3, "blocked": 4, "ineligible": 5,
		"missingTask": 6, "missingEmail": 7, "excludedPlan": 8, "busy": 9,
		"cooldown": 10, "outsideGroups": 11,
	}
	patterns := []string{
		`检查\s*41\s*条`, `已排队自动重登\s*2\s*条`, `已更新(?:账号)?\s*3\s*条`,
		`永久跳过\s*4\s*条`, `需人工(?:处理)?\s*5\s*条`, `本地无任务\s*6\s*条`,
		`(?:缺少邮箱|邮箱缺失)[^0-9]*7\s*条`, `按套餐(?:排除|跳过)[^0-9]*8\s*条`,
		`正在(?:运行|处理)\s*9\s*条`, `冷却中\s*10\s*条`,
		`(?:分组外|分组跳过|不在所选分组|不在监控分组|非目标分组)[^0-9]*11\s*条`,
	}
	for _, lastError := range []string{"", "synthetic partial failure"} {
		got := presentMonitorResult(t, vm, result, lastError)
		for _, pattern := range patterns {
			if !regexp.MustCompile(pattern).MatchString(got.Text) {
				t.Errorf("result text lost count %q (error=%q): %s", pattern, lastError, got.Text)
			}
		}
	}
}

func TestMonitorPresentationRestoresLastResultAndErrorAfterRefresh(t *testing.T) {
	vm := presentationVM(t)
	for _, tc := range []struct {
		name, lastError, code string
		result                any
	}{
		{"excluded result", "", "excluded", map[string]any{"checked": 1, "excludedPlan": 1}},
		{"queued result", "", "queued", map[string]any{"checked": 3, "started": 2, "excludedPlan": 1}},
		{"partial result", "", "partial", map[string]any{"checked": 1, "updated": 2, "excludedPlan": 1}},
		{"nested result error", "", "error", map[string]any{"updated": 2, "error": "synthetic result failure"}},
		{"state error takes priority", "synthetic state failure", "error", map[string]any{"updated": 2, "error": "synthetic result failure"}},
		{"state save error without result error", "synthetic state save failure", "error", map[string]any{"updated": 2}},
		{"no previous result", "", "empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := presentMonitorResult(t, vm, tc.result, tc.lastError)
			for _, running := range []bool{false, true} {
				var got struct{ ResultPresentation monitorResultPresentation }
				monitorPresentationCall(t, vm, "describeMonitor", &got, map[string]any{
					"configured": true, "enabled": true, "running": running,
					"lastResult": tc.result, "lastError": tc.lastError,
				})
				if got.ResultPresentation.Code != tc.code || !reflect.DeepEqual(got.ResultPresentation, want) {
					t.Fatalf("persisted result changed on refresh (running=%v): got=%+v want=%+v", running, got.ResultPresentation, want)
				}
			}
		})
	}
}
