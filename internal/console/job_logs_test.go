package console

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

type displayedLog struct {
	Raw, Operation, OperationAt, Kind, Label, Message, Key string
	Abnormal                                               bool
}

func jobLogsVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	built := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   `export { parseJobLogs } from "./web/src/components/job-logs.jsx";`,
			ResolveDir: root, Sourcefile: "job-logs-fixture.js",
		},
		Bundle: true, Write: false, TreeShaking: api.TreeShakingTrue,
		Format: api.FormatIIFE, GlobalName: "JobLogs", Platform: api.PlatformBrowser,
		Alias: map[string]string{
			"react":        filepath.Join(root, "web/vendor/react/react.production.js"),
			"lucide-react": filepath.Join(root, "web/vendor/lucide-react/index.js"),
		},
		Define: map[string]string{"process.env.NODE_ENV": `"production"`},
		Loader: map[string]api.Loader{".jsx": api.LoaderJSX, ".css": api.LoaderEmpty},
	})
	if len(built.Errors) > 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("log parser bundle failed: %v", built.Errors)
	}
	vm := sobek.New()
	// Date formatting is exercised by the browser; these tests cover the data
	// used for filtering, grouping, and distinguishing operation outcomes.
	if _, err := vm.RunString(`var Intl = {DateTimeFormat: function () {this.format = function (date) {return date.toISOString();};}};`); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(string(built.OutputFiles[0].Contents)); err != nil {
		t.Fatal(err)
	}
	return vm
}

func parseLogsFixture(t *testing.T, vm *sobek.Runtime, logs any, entries []map[string]any) []displayedLog {
	t.Helper()
	fn, ok := sobek.AssertFunction(vm.Get("JobLogs").ToObject(vm).Get("parseJobLogs"))
	if !ok {
		t.Fatal("missing parseJobLogs export")
	}
	result, err := fn(sobek.Undefined(), vm.ToValue(logs), vm.ToValue(entries))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result.Export())
	if err != nil {
		t.Fatal(err)
	}
	var out []displayedLog
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestJobLogsMultilineEventsDoNotDuplicateOrRemoveHistory(t *testing.T) {
	vm := jobLogsVM(t)
	entry := map[string]any{
		"at": "2026-10-12T08:10:11.123456789Z", "operationAt": "2026-10-12T08:10:10.123456789Z",
		"operation": "relogin", "stage": "login", "status": "working", "operationStatus": "running",
		"level": "info", "message": "第一行\n第二行 [REDACTED]",
	}
	formatted := "2026-10-12T08:10:11Z [info] 第一行\n第二行 [REDACTED]"
	oldTimestamped := "2026-10-11T08:10:11Z [info] earlier event"
	got := parseLogsFixture(t, vm, "login running\n"+oldTimestamped+"\n"+formatted, []map[string]any{entry})
	if len(got) != 3 || got[0].OperationAt != "" || got[1].Raw != oldTimestamped || got[2].Message != entry["message"] || !strings.Contains(got[2].Raw, "[REDACTED]") {
		t.Fatalf("multiline event duplicated, fragmented, or lost history: %+v", got)
	}
	got = parseLogsFixture(t, vm, formatted, []map[string]any{entry})
	if len(got) != 1 || got[0].Message != entry["message"] {
		t.Fatalf("single structured event acquired extra physical-line records: %+v", got)
	}
	entry["at"], entry["message"] = "2026-10-12T16:10:11.123456789+08:00", "第一行\r\n第二行"
	got = parseLogsFixture(t, vm, "2026-10-12T16:10:11+08:00 [info] 第一行\r\n第二行", []map[string]any{entry})
	if len(got) != 1 {
		t.Fatalf("offset timestamp or CRLF duplicated event: %+v", got)
	}
}

func TestJobLogsKeepLegacyRecordsAndStableOccurrenceKeys(t *testing.T) {
	vm := jobLogsVM(t)
	for _, entries := range [][]map[string]any{nil, {}} {
		got := parseLogsFixture(t, vm, "login running\nfailed failed\nlogin running", entries)
		if len(got) != 3 || got[0].Kind != "progress" || got[1].Kind != "error" || got[0].Key == got[2].Key {
			t.Fatalf("legacy fallback or duplicate keys failed: %+v", got)
		}
	}
	var raw []string
	var entries []map[string]any
	for i := range 100 {
		raw = append(raw, "login running")
		entries = append(entries, map[string]any{"at": "2026-10-12T08:10:11Z", "level": "info", "message": fmt.Sprintf("合成事件 %d", i)})
	}
	for _, entry := range entries {
		raw = append(raw, fmt.Sprintf("2026-10-12T08:10:11Z [info] %s", entry["message"]))
	}
	got := parseLogsFixture(t, vm, strings.Join(raw, "\n"), entries)
	if len(got) != 200 {
		t.Fatalf("removed counterparts consumed the history limit: %d logs", len(got))
	}
	if got[0].Raw != "login running" || got[99].Raw != "login running" || got[100].Message != "合成事件 0" {
		t.Fatalf("old history was lost or reordered: first %+v, final old %+v, first new %+v", got[0], got[99], got[100])
	}
}

func TestJobLogsPrioritizeOperationOutcomeOverRetainedAccountState(t *testing.T) {
	vm := jobLogsVM(t)
	cases := []struct {
		name, status, outcome, level, message, kind string
		abnormal                                    bool
	}{
		{"failed retains completed grant", "completed", "failed", "success", "合成失败", "error", true},
		{"canceled retains completed grant", "completed", "canceled", "success", "合成取消", "warning", true},
		{"cancellation overrides prior error level", "completed", "canceled", "error", "合成取消", "warning", true},
		{"interrupted retains resume checkpoint", "resume_available", "interrupted", "success", "合成中断", "warning", true},
		{"successful account operation retains imported state", "imported", "succeeded", "success", "操作完成", "success", false},
		{"successful account operation retains completed state", "completed", "succeeded", "success", "操作完成", "success", false},
		{"success level supports legacy imported result", "imported", "", "success", "操作完成", "success", false},
		{"legacy grant completion", "completed", "", "info", "授权完成", "success", false},
		{"message does not determine failure", "working", "running", "info", "error count 0", "progress", false},
		{"waiting supersedes retained completion", "completed", "waiting", "info", "等待输入", "waiting", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLogsFixture(t, vm, "", []map[string]any{{"status": tc.status, "operationStatus": tc.outcome, "level": tc.level, "message": tc.message}})
			if len(got) != 1 || got[0].Kind != tc.kind || got[0].Abnormal != tc.abnormal {
				t.Fatalf("incorrect log outcome: %+v", got)
			}
		})
	}
	got := parseLogsFixture(t, vm, "", []map[string]any{{"stage": "login", "status": "completed", "operationStatus": "canceled", "message": "synthetic cancellation"}})
	if len(got) != 1 || !strings.Contains(got[0].Message, "已取消") || strings.Contains(got[0].Message, "已中断") {
		t.Fatalf("cancellation incorrectly described as interruption: %+v", got)
	}
}

func TestJobLogsPreserveNanosecondsAndNormalizeInitialAuthorization(t *testing.T) {
	vm := jobLogsVM(t)
	first := "2026-10-12T08:10:10.123456001Z"
	second := "2026-10-12T08:10:10.123456999Z"
	got := parseLogsFixture(t, vm, "", []map[string]any{
		{"operation": "full", "operationAt": first, "message": "第一次授权"},
		{"operation": "relogin", "operationAt": second, "message": "第二次授权"},
	})
	if len(got) != 2 || got[0].Operation != "initial_authorization" || got[0].OperationAt != first || got[1].OperationAt != second || got[0].OperationAt == got[1].OperationAt {
		t.Fatalf("nanosecond grouping or initial authorization label was lost: %+v", got)
	}
}

func TestJobLogsLogoutDiagnosticsPreserveStatusWithoutSuggestingReplay(t *testing.T) {
	vm := jobLogsVM(t)
	for _, tc := range []struct {
		name, stage, status, message, kind, detail string
	}{
		{"HTTP rejection", "totp_logout_failed", "failed", "logout all devices failed: HTTP 400: invalid_request", "error", "HTTP 400"},
		{"HTTP outcome unknown", "totp_logout_unknown", "unknown", "logout all devices outcome unknown: HTTP status 502", "warning", "HTTP 502"},
		{"timeout outcome unknown", "totp_logout_unknown", "unknown", "logout all devices outcome unknown: context deadline exceeded", "warning", "请求超时"},
		{"request timeout outcome unknown", "totp_logout_unknown", "unknown", "logout all devices outcome unknown: request timed out", "warning", "请求超时"},
		{"proxy timeout outcome unknown", "totp_logout_unknown", "unknown", "logout all devices outcome unknown: proxy connection timed out", "warning", "请求超时"},
		{"cancellation outcome unknown", "totp_logout_unknown", "unknown", "logout all devices outcome unknown: context canceled", "warning", "请求已取消"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLogsFixture(t, vm, "", []map[string]any{{"stage": tc.stage, "status": tc.status, "level": "error", "message": tc.message}})
			if len(got) != 1 || got[0].Kind != tc.kind || !strings.Contains(got[0].Message, tc.detail) || !strings.Contains(got[0].Message, "不会自动重试") || strings.Contains(got[0].Message, "检查网络后重试") || !strings.Contains(got[0].Raw, tc.message) {
				t.Fatalf("logout diagnostic lost status, suggests replay, or removed original cause: %+v", got)
			}
			if tc.stage == "totp_logout_unknown" && (!strings.Contains(got[0].Message, "尚未确认") || !strings.Contains(got[0].Message, "先核实")) {
				t.Fatalf("unknown logout result implies confirmed failure: %+v", got)
			}
		})
	}

	message := "退出所有设备失败（HTTP 400），新 2FA 已保存；后续授权和上传已停止，不会自动重试。"
	got := parseLogsFixture(t, vm, "", []map[string]any{{"stage": "totp_logout_failed", "status": "failed", "message": message}})
	if len(got) != 1 || got[0].Message != message {
		t.Fatalf("new Chinese logout diagnostic was replaced or duplicated: %+v", got)
	}
}

func TestJobLogsRequestTimeoutIsVisibleWithKnownStageAndInterruptedHistory(t *testing.T) {
	vm := jobLogsVM(t)
	for _, reason := range []string{"request timed out", "proxy connection timed out", "context deadline exceeded"} {
		for _, status := range []string{"failed", "interrupted"} {
			t.Run(reason+"/"+status, func(t *testing.T) {
				got := parseLogsFixture(t, vm, "", []map[string]any{{"stage": "legacy_session_start", "operationStatus": status, "level": "error", "message": reason}})
				if len(got) != 1 || !got[0].Abnormal || !strings.Contains(got[0].Message, "请求超时") || strings.Contains(got[0].Message, "已中断") || !strings.Contains(got[0].Raw, reason) {
					t.Fatalf("timeout reason hidden by known stage or interruption: %+v", got)
				}
			})
		}
	}
}

func TestJobLogsRetainChineseNetworkRetryDetails(t *testing.T) {
	vm := jobLogsVM(t)
	for _, tc := range []struct {
		status, message, kind string
	}{
		{"running", "网络请求超时；第 1/3 次重试，正在重新连接", "progress"},
		{"failed", "网络请求超时，已重试3次，请检查网络后重新执行", "error"},
	} {
		got := parseLogsFixture(t, vm, "", []map[string]any{{"stage": "legacy_session_start", "operationStatus": tc.status, "message": tc.message}})
		if len(got) != 1 || got[0].Kind != tc.kind || got[0].Message != tc.message {
			t.Fatalf("retry progress or exhaustion summary was replaced: %+v", got)
		}
	}
}
