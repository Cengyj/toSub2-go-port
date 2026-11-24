package console

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/resetlog"
)

var resetPages = flag.String("reset-public-pages", "", "read sanitized pages produced by TestResetConsoleRequestLedger")

func copyResetPage(t *testing.T, page string) (string, string) {
	t.Helper()
	vm := frontendInsightsVM(t, "components/job-logs.jsx")
	if e := vm.Set("apiBody", page); e != nil {
		t.Fatal(e)
	}
	frontendPollingRun(t, vm, `Intl.DateTimeFormat=function(){return {format(date){return date.toISOString()}}};`)
	frontendPollingRun(t, vm, `var copied=""; var navigator={clipboard:{async writeText(text){copied=text;}}};var page=JSON.parse(apiBody);var last=page.entries.filter(e=>e.reset_diagnostic?.terminal).pop();var jobStatus=page.entries.filter(e=>e.operationAt===last.operationAt&&e.status).pop()?.status;var props={jobId:page.id,token:"fixture",job:{status:jobStatus || "failed",lastOperationAt:last.operationAt}};var tree=renderComponent("JobLogs",props);`)
	frontendPollingRun(t, vm, `assert(requests.length===1,"log page not fetched");ok(requests[0],page);`)
	frontendPollingRun(t, vm, `tree=renderComponent("JobLogs",props);findButton(tree,"全部").props.onClick();tree=renderComponent("JobLogs",props);var copyButton=()=>findMatching(tree,n=>n.props?.["aria-label"]==="复制当前显示的日志");copyButton().props.onClick();`)
	full := vm.Get("copied").String()
	frontendPollingRun(t, vm, `findMatching(tree,n=>n.type==="button"&&content(n).startsWith("异常")).props.onClick();tree=renderComponent("JobLogs",props);copyButton().props.onClick();`)
	return full, vm.Get("copied").String()
}

func TestFrontendResetClipboardKeyAvailability(t *testing.T) {
	const marker = "synthetic-private-key-availability-canary"
	at := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	details := []resetlog.Detail{
		{Stage: "pending", Reason: "PENDING_VERIFIED", Pending: "verified"},
		{Stage: "activation", Reason: "ACTIVATION_RECEIPT_CONFIRMED", Pending: "verified", Activation: "accepted", Match: "skipped"},
		{Stage: "local_commit", Reason: "RESET_SAVED_BY_RECEIPT", Terminal: true, LastTrusted: "local_commit", Pending: "verified", Activation: "accepted", Match: "skipped", Commit: "committed", Removal: "not_performed", Logout: "not_requested"},
	}
	var entries []jobs.LogEntry
	for i, detail := range details {
		detail.OpRef, detail.Seq = "O0123456789abcdef", i+1
		// Exercise the public event boundary before rendering and copying. An
		// arbitrary sibling or prompt must not become part of a diagnostic.
		event := jobs.SanitizeJobEvent(jobs.JobEvent{Prompt: marker, Data: map[string]any{resetlog.DataKey: &detail, "secret": marker}})
		entries = append(entries, jobs.LogEntry{At: at.Add(time.Duration(i) * time.Second), OperationAt: at, Stage: event.Stage, Message: event.Prompt, Level: detail.Level(), Diagnostic: event.Data[resetlog.DataKey].(*resetlog.Detail)})
	}
	page, err := json.Marshal(map[string]any{"id": "key-availability", "logs": "", "entries": entries, "hasMore": false})
	if err != nil {
		t.Fatal(err)
	}
	full, _ := copyResetPage(t, string(page))
	for _, want := range []string{"尚未激活，暂不可领取", "本地最终保存尚未完成", "可在“账号信息 → 2FA 密钥”查看", "未另查验证器列表"} {
		if !strings.Contains(full, want) {
			t.Fatalf("copied progress does not explain key availability: missing %s", want)
		}
	}
	if strings.Contains(full, marker) {
		t.Fatal("private synthetic material escaped into clipboard")
	}
	vm := jobLogsVM(t)
	var rendered []map[string]any
	encoded, _ := json.Marshal(entries)
	if err := json.Unmarshal(encoded, &rendered); err != nil {
		t.Fatal(err)
	}
	visible := parseLogsFixture(t, vm, "", rendered)
	if len(visible) != 3 || !strings.Contains(visible[0].Message, "暂不可领取") || !strings.Contains(visible[1].Message, "本地最终保存尚未完成") || !strings.Contains(visible[2].Message, "账号信息 → 2FA 密钥") {
		t.Fatal("visible log panel differs from the key availability explanation in copied text")
	}
}

// Exercise the real component, fetch hook, filter and clipboard button. No
// journal, HAR or vault is input to this side of the test.
func TestFrontendResetClipboard(t *testing.T) {
	page := `{"id":"one","logs":"","entries":[{"at":"2026-11-18T01:00:00Z","operationAt":"2026-11-18T00:59:59Z","stage":"totp_reset_diagnostic","level":"warning","message":"当前查询未匹配本次因子","reset_diagnostic":{"op_ref":"O0123456789abcdef","seq":12,"reason_code":"EXPECTED_FACTOR_NOT_VISIBLE","terminal":true,"last_trusted_step":"activation","activation_receipt":"accepted","factor_match":"not_visible","pending_readback":"verified","next_action":"review_binding","expected_factor":"F2","visible_factor":"F3","writes_this_run":{"activate":{"received":1,"possible":0}}}}],"hasMore":true,"nextBefore":"older"}`
	if *resetPages != "" {
		b, e := os.ReadFile(filepath.Join(*resetPages, "new-not-visible.json"))
		if e != nil {
			t.Fatal(e)
		}
		page = string(b)
	}
	full, abnormal := copyResetPage(t, page)
	wantReceipt, wantLast, wantCount := "accepted", "activation", `"activate":{"received":1`
	if *resetPages != "" { // Live read after a lost receipt. The embedded fixture above is historical.
		wantReceipt, wantLast, wantCount = "unknown", "pending", `"activate":{"received":0,"possible":1`
	}
	for _, field := range []string{"复制范围：本次", "筛选：全部", "北京时间", "+08:00", "EXPECTED_FACTOR_NOT_VISIBLE", `"last_trusted_step":"` + wantLast + `"`, `"pending_readback":"verified"`, wantCount, `"next_action":"review_binding"`} {
		if !strings.Contains(full, field) {
			t.Fatalf("clipboard missing %s", field)
		}
	}
	// Verify the answer from copied text, not from a manager object or test oracle.
	var terminal struct {
		Detail struct {
			Op         string `json:"op_ref"`
			Terminal   bool   `json:"terminal"`
			Expected   string `json:"expected_factor"`
			Visible    string `json:"visible_factor"`
			Activation string `json:"activation_receipt"`
		} `json:"reset_diagnostic"`
	}
	found := 0
	for _, line := range strings.Split(full, "\n") {
		var rec = terminal
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Detail.Terminal {
			found++
			if rec.Detail.Op == "" || rec.Detail.Expected != "F2" || rec.Detail.Visible != "F3" || rec.Detail.Activation != wantReceipt {
				t.Error("copy does not distinguish the activation receipt from the mismatched factor read")
			}
		}
	}
	if found != 1 {
		t.Fatal("expected exactly one terminal summary", found)
	}
	if !strings.Contains(abnormal, "筛选：异常") {
		t.Fatal("filtered copy claims full coverage")
	}
	if *resetPages != "" {
		if err := os.WriteFile(filepath.Join(*resetPages, "clipboard.txt"), []byte(full+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		pages, e := filepath.Glob(filepath.Join(*resetPages, "*.json"))
		if e != nil {
			t.Fatal(e)
		}
		var index strings.Builder
		for _, p := range pages {
			b, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			text, warnings := copyResetPage(t, string(b))
			for _, marker := range []string{"private-provider-canary", "synthetic-session-projection-canary", "synthetic-enrollment-session", "synthetic-password", "fixture-canary", "extension-canary", "other-canary", "wrong-account-canary", "wrong-readback-canary", "otpauth://"} {
				if strings.Contains(text, marker) || strings.Contains(warnings, marker) {
					t.Fatal("private synthetic material escaped into copied logs", filepath.Base(p))
				}
			}
			if strings.Contains(text, "新 2FA 密钥已安全保存") {
				t.Fatal("copied activation progress prematurely claimed final credential storage", filepath.Base(p))
			}
			n := 0
			for _, line := range strings.Split(text, "\n") {
				var rec struct {
					Message string `json:"message"`
					Detail  struct {
						Terminal bool                      `json:"terminal"`
						Op       string                    `json:"op_ref"`
						Reason   string                    `json:"reason_code"`
						Cause    string                    `json:"cause_reason"`
						Last     string                    `json:"last_trusted_step"`
						Next     string                    `json:"next_action"`
						Removal  string                    `json:"old_removal"`
						Commit   string                    `json:"local_commit"`
						Match    string                    `json:"factor_match"`
						Receipt  string                    `json:"activation_receipt"`
						Logout   string                    `json:"logout"`
						Writes   map[string]map[string]int `json:"writes_this_run"`
					} `json:"reset_diagnostic"`
				}
				if json.Unmarshal([]byte(line), &rec) == nil && rec.Detail.Terminal {
					if rec.Detail.Op == "" || rec.Detail.Last == "" || rec.Detail.Next == "" {
						t.Fatal("copy lacks operation, checkpoint or next step", p)
					}
					if rec.Detail.Commit == "committed" && !strings.Contains(rec.Message, "可在“账号信息 → 2FA 密钥”查看") {
						t.Fatal("copied result omitted the protected key retrieval entry point", filepath.Base(p))
					}
					if rec.Detail.Reason == "TARGET_UNRESOLVED" || rec.Detail.Reason == "TARGET_NOT_AUTHORIZED" {
						removal := "not_performed"
						if filepath.Base(p) == "removed-conflict-then-multiple.json" {
							removal = "accepted"
							if !strings.HasPrefix(rec.Message, "本轮结果：旧因子移除已有证据，新因子尚未确认。") || strings.Contains(rec.Message, "未发送移除") || strings.Contains(rec.Message, "未移除任何验证器") {
								t.Fatal("copied target uncertainty erased the confirmed old removal", p)
							}
						}
						if rec.Detail.Next != "review_binding" || rec.Detail.Removal != removal || !strings.Contains(rec.Message, "请在账号安全设置中核对") {
							t.Fatal("copy turned ambiguous state into a removal", p)
						}
					}
					if strings.HasPrefix(filepath.Base(p), "local-only") || filepath.Base(p) == "legacy-binding-write.json" || filepath.Base(p) == "legacy-binding-read.json" {
						for _, step := range []string{"remove", "initial_enroll", "enroll", "activate", "logout"} {
							for _, kind := range []string{"received", "possible", "unsent"} {
								if n, ok := rec.Detail.Writes[step][kind]; !ok || n != 0 {
									t.Fatal("copy lacks zero network writes for local completion", p, step, kind)
								}
							}
						}
					}
					switch filepath.Base(p) {
					case "legacy-binding-write.json", "legacy-binding-read.json", "active-read.json", "final-metadata.json":
						want := "STORAGE_WRITE_FAILED"
						if filepath.Base(p) == "legacy-binding-read.json" || filepath.Base(p) == "active-read.json" {
							want = "STORAGE_READBACK_FAILED"
						} else if filepath.Base(p) == "final-metadata.json" {
							want = "STORAGE_COMMIT_FAILED"
						}
						if rec.Detail.Reason != "RECEIPT_LOCAL_COMMIT_REQUIRED" || rec.Detail.Cause != want || rec.Detail.Receipt != "accepted" || rec.Detail.Last != "activation" || rec.Detail.Commit != "failed" || rec.Detail.Next != "complete_local_only" || !strings.Contains(warnings, line) {
							t.Fatal("filtered copy lost the specific vault fault, confirmed activation, or offline next step", p)
						}
						if rec.Detail.Writes["logout"]["received"] != 0 || rec.Detail.Writes["logout"]["possible"] != 0 {
							t.Fatal("uncommitted key result includes a logout request", p)
						}
					case "activation-mixed-signals-logout.json":
						if rec.Detail.Reason != "EXPECTED_FACTOR_NOT_VISIBLE" || rec.Detail.Receipt != "unknown" || rec.Detail.Commit == "committed" || rec.Detail.Logout != "not_performed" || !strings.Contains(line, `"last_write_issue":"CONTRADICTORY_RECEIPT"`) || !strings.Contains(warnings, "回包同时表示激活和停用") {
							t.Fatal("clipboard lost the contradictory write receipt or claimed commit/logout", p)
						}
					case "old.json", "empty.json", "verified-old-logout.json", "removed-conflict-then-disabled.json", "conflict-then-disabled.json", "local-only.json", "local-only-active-read.json", "local-only-final-metadata.json", "checkpoint-only.json", "already-bound.json", "legacy-binding.json":
						if rec.Detail.Match != "skipped" || rec.Detail.Receipt != "accepted" || rec.Detail.Commit != "committed" || !strings.Contains(rec.Message, "网站已确认激活，新密钥已保存；未另查验证器列表") || strings.Contains(text, `"stage":"verify_factor"`) {
							t.Fatal("copy invented a list match or lost receipt-only completion", p)
						}
					case "empty-logout-unconfirmed.json", "empty-logout-timeout.json":
						if rec.Detail.Reason != "RESET_COMPLETE_LOGOUT_UNCONFIRMED" || rec.Detail.Next != "review_logout" || rec.Detail.Logout != "unknown" || rec.Detail.Commit != "committed" || !strings.Contains(rec.Message, "新密钥已保存；未另查验证器列表") || !strings.Contains(rec.Message, "未确认退出结果") || !strings.Contains(warnings, line) || strings.Contains(warnings, "退出所有设备未完成") {
							t.Fatal("clipboard failed to distinguish a saved key from unconfirmed logout", p)
						}
						if filepath.Base(p) == "empty-logout-timeout.json" && (!strings.Contains(warnings, "REQUEST_TIMEOUT") || !strings.Contains(warnings, "未收到 HTTP 响应") || rec.Detail.Writes["logout"]["possible"] != 1 || rec.Detail.Writes["logout"]["received"] != 0) {
							t.Fatal("clipboard lost the unanswered logout write while retaining the new key", p)
						}
					case "new-and-old.json", "new-and-old-logout.json", "new-and-old-logout-unconfirmed.json", "new-and-other.json", "local-only-with-other.json":
						if rec.Detail.Reason != "RESET_SAVED_OTHER_VISIBLE" || rec.Detail.Next != "review_binding" || rec.Detail.Commit != "committed" || !strings.Contains(warnings, line) || !strings.Contains(rec.Message, "新密钥已核对并保存") {
							t.Fatal("remaining authenticator hidden in the current warning view", p)
						}
						if filepath.Base(p) == "new-and-old-logout.json" && (rec.Detail.Logout != "accepted" || !strings.Contains(rec.Message, "退出请求已接受")) {
							t.Fatal("logout receipt overwrote or hid the factor warning", p)
						}
					}
					n++
					fmt.Fprintf(&index, "%s\t%s\n", strings.TrimSuffix(filepath.Base(p), ".json"), line)
				}
			}
			if n != 1 {
				t.Fatal("wrong number of terminal records in current UI scope", filepath.Base(p), n)
			}
			warningFile := ""
			switch filepath.Base(p) {
			case "old.json":
				if err := os.WriteFile(filepath.Join(*resetPages, "clipboard-receipt.txt"), []byte(text+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "new-and-old-logout.json":
				warningFile = "clipboard-warning.txt"
			case "empty-logout-unconfirmed.json":
				warningFile = "clipboard-logout-unconfirmed.txt"
			case "empty-logout-timeout.json":
				warningFile = "clipboard-logout-timeout.txt"
			case "removed-conflict-then-multiple.json":
				warningFile = "clipboard-removed-target-uncertain.txt"
			case "activation-mixed-signals-logout.json":
				warningFile = "clipboard-mixed-signals.txt"
			case "legacy-binding-read.json":
				warningFile = "clipboard-local-readback.txt"
			}
			if warningFile != "" {
				if err := os.WriteFile(filepath.Join(*resetPages, warningFile), []byte(warnings+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		if e := os.WriteFile(filepath.Join(*resetPages, "clipboard-terminals.txt"), []byte(index.String()), 0600); e != nil {
			t.Fatal(e)
		}
	} else if !strings.Contains(full, "尚有更早页") {
		t.Fatal("partial page not labelled")
	}
}
