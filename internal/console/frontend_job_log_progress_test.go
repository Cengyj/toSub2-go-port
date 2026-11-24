package console

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"tosub2-go-port/internal/resetlog"
)

func normalLogRead() map[string]any {
	return map[string]any{
		"at": "2026-11-24T02:00:00Z", "operationAt": "2026-11-24T01:59:00Z", "operation": "reset-2fa",
		"stage": "totp_reset_diagnostic", "status": "working", "operationStatus": "running", "level": "info", "message": "已收到完整 HTTP 回包，业务结果另行判断（HTTP 200）",
		"reset_diagnostic": map[string]any{"op_ref": "O0123456789abcdef", "seq": 2, "attempt_ref": "A1", "stage": "initial_state", "reason_code": "REQUEST_COMPLETE", "evidence_source": "transport", "next_action": "classify_receipt", "method": "GET", "path": "/backend-api/accounts/mfa_info", "dispatch": "response_received", "http_status": 200, "receipt": "complete", "duration_ms": 0, "access": "read"},
	}
}

func TestJobLogsRoutineReadsRequireConsistentStructuredEvidence(t *testing.T) {
	vm := jobLogsVM(t)
	base := normalLogRead()
	got := parseLogsFixture(t, vm, "", []map[string]any{base})
	if !got[0].RoutineNetwork || got[0].Kind == "success" || got[0].Message != "读取 2FA 设置：收到完整响应（HTTP 200 · 0 ms）；待解析业务结果" {
		t.Fatal("normal network observation lost timing or became business success", got)
	}
	for _, status := range []int{200, 204, 302, 399} {
		entry := normalLogRead()
		entry["reset_diagnostic"].(map[string]any)["http_status"] = status
		if !parseLogsFixture(t, vm, "", []map[string]any{entry})[0].RoutineNetwork {
			t.Fatal("complete normal read cannot be filtered", status)
		}
	}
	cases := []struct {
		name, field string
		value       any
		outer       bool
	}{
		{"missing operation", "op_ref", nil, false}, {"invalid operation", "op_ref", "email@example.invalid", false},
		{"array operation", "op_ref", []string{"O0123456789abcdef"}, false},
		{"missing attempt", "attempt_ref", nil, false}, {"invalid attempt", "attempt_ref", "A0", false},
		{"array attempt", "attempt_ref", []string{"A1"}, false},
		{"missing sequence", "seq", nil, false}, {"bad sequence", "seq", -1, false},
		{"missing read evidence", "access", nil, false}, {"write", "access", "write", false},
		{"POST is not read", "method", "POST", false}, {"missing method", "method", nil, false},
		{"mutation GET", "path", "/backend-api/accounts/logout_all", false}, {"unknown path", "path", "/unknown", false},
		{"prototype path", "path", "constructor", false},
		{"array path", "path", []string{"/backend-api/accounts/mfa_info"}, false},
		{"missing path", "path", nil, false}, {"conflicting stage", "stage", "activation", false},
		{"wrong next action", "next_action", "review_receipt", false},
		{"HTTP refusal", "http_status", 403, false}, {"server error", "http_status", 503, false}, {"missing HTTP", "http_status", nil, false},
		{"string HTTP", "http_status", "200", false}, {"fraction HTTP", "http_status", 200.5, false},
		{"intent", "reason_code", "DISPATCH_INTENT", false}, {"unknown", "reason_code", "NO_RESPONSE", false},
		{"unsent", "dispatch", "proven_unsent", false}, {"possibly sent", "dispatch", "possibly_sent", false},
		{"incomplete", "receipt", "incomplete", false}, {"missing receipt", "receipt", nil, false},
		{"nontransport", "evidence_source", "business_receipt", false}, {"terminal", "terminal", true, false},
		{"business error", "business_code", "invalid_code", false}, {"cause", "cause_reason", "STORAGE_WRITE_FAILED", false},
		{"write issue", "last_write_issue", "CONTRADICTORY_RECEIPT", false}, {"unknown extension", "future_evidence", false, false},
		{"pending material", "pending_readback", "verified", false},
		{"negative duration", "duration_ms", -1, false}, {"string duration", "duration_ms", "653", false},
		{"invalid start", "started_at", "invalid", false}, {"invalid end", "ended_at", false, false},
		{"wrong outer stage", "stage", "totp_reset_complete", true}, {"warn", "level", "warning", true}, {"error", "level", "error", true},
		{"unknown level", "level", "future", true}, {"missing level", "level", nil, true},
	}
	for _, field := range []string{"status", "operationStatus"} {
		for _, status := range []string{"failed", "unknown", "interrupted", "canceled"} {
			cases = append(cases, struct {
				name, field string
				value       any
				outer       bool
			}{field + "/" + status, field, status, true})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := normalLogRead()
			target := entry["reset_diagnostic"].(map[string]any)
			if tc.outer {
				target = entry
			}
			if tc.value == nil {
				delete(target, tc.field)
			} else {
				target[tc.field] = tc.value
			}
			shown := parseLogsFixture(t, vm, "", []map[string]any{entry})
			if len(shown) != 1 || shown[0].RoutineNetwork {
				t.Fatal("uncertain or important evidence was classified as routine", shown)
			}
			if tc.outer && (tc.field == "status" || tc.field == "operationStatus" || tc.field == "level") && shown[0].Message != entry["message"] {
				t.Fatal("transport summary replaced an outcome-bearing message", shown)
			}
		})
	}
}

func TestJobLogsHistoricRequestSummaryMatchesServerMessage(t *testing.T) {
	vm := jobLogsVM(t)
	for _, tc := range []struct{ stage, path, method string }{
		{"identity", "/api/auth/session", "GET"}, {"initial_state", "/backend-api/accounts/mfa_info", "GET"},
		{"conflict_read", "/backend-api/accounts/mfa_info", "GET"}, {"verify_disable", "/backend-api/accounts/mfa_info", "GET"}, {"verify_factor", "/backend-api/accounts/mfa_info", "GET"},
		{"disable", "/backend-api/accounts/mfa/user/disable_in_house", "POST"},
		{"initial_enroll", "/backend-api/accounts/mfa/enroll", "POST"}, {"enroll", "/backend-api/accounts/mfa/enroll", "POST"}, {"retry_enroll", "/backend-api/accounts/mfa/enroll", "POST"},
		{"activation", "/backend-api/accounts/mfa/user/activate_enrollment", "POST"}, {"logout", "/backend-api/accounts/logout_all", "POST"},
		{"login", "/", "GET"}, {"login", "/", "POST"}, {"login", "/:login", "HEAD"}, {"login", "/:login", "POST"},
		{"verify_factor", "", "GET"}, {"login", "", "POST"},
	} {
		for _, duration := range []*int64{nil, new(int64(0)), new(int64(653)), new(int64(-1))} {
			d := resetlog.Detail{OpRef: "O0123456789abcdef", Seq: 2, AttemptRef: "A1", Stage: tc.stage, Reason: "REQUEST_COMPLETE", Source: "transport", Method: tc.method, Path: tc.path, Dispatch: "response_received", HTTP: 200, Receipt: "complete", DurationMS: duration}
			encoded, _ := json.Marshal(d)
			var detail map[string]any
			if err := json.Unmarshal(encoded, &detail); err != nil {
				t.Fatal(err)
			}
			entry := normalLogRead()
			entry["reset_diagnostic"] = detail
			got := parseLogsFixture(t, vm, "", []map[string]any{entry})
			if got[0].Message != d.Message() || !strings.Contains(got[0].Raw, entry["message"].(string)) {
				t.Fatalf("historical projection differs from server or lost raw: %s %s: got %q want %q", tc.stage, tc.path, got[0].Message, d.Message())
			}
		}
	}
}

func TestJobLogsExplicitStepCompletionDoesNotFinishWorkflow(t *testing.T) {
	vm := jobLogsVM(t)
	for _, stage := range []string{"totp_workflow_relogin", "totp_workflow_upload"} {
		entry := map[string]any{"stage": stage, "status": "succeeded", "operationStatus": "running", "message": "本步骤已有明确结果", "level": "info"}
		got := parseLogsFixture(t, vm, "", []map[string]any{entry})
		if got[0].Kind != "success" || got[0].Label != "步骤完成" || got[0].Message != entry["message"] {
			t.Fatal("confirmed substep lost its independent result", got)
		}
		for _, status := range []string{"failed", "unknown", "canceled", "interrupted"} {
			entry["operationStatus"] = status
			if got := parseLogsFixture(t, vm, "", []map[string]any{entry}); !got[0].Abnormal || got[0].Kind == "success" {
				t.Fatal("substep replaced enclosing failure or uncertainty", got)
			}
		}
	}
	entry := normalLogRead()
	entry["reset_diagnostic"].(map[string]any)["terminal"] = true
	entry["message"] = "本轮结果：新密钥已保存；后续重新授权另行记录"
	got := parseLogsFixture(t, vm, "", []map[string]any{entry})
	if got[0].PhaseLabel != "2FA步骤结果" || got[0].RoutineNetwork || got[0].Message != entry["message"] {
		t.Fatal("terminal 2FA result became a completed combination workflow", got)
	}
}

func TestJobLogsTransportObservationCannotClaimBusinessSuccess(t *testing.T) {
	vm := jobLogsVM(t)
	for _, fields := range []map[string]any{
		{"status": "completed", "operationStatus": "succeeded"},
		{"status": "completed", "operationStatus": "", "level": "success"},
		{"status": "working", "operationStatus": "running", "level": "success"},
	} {
		entry := normalLogRead()
		for key, value := range fields {
			entry[key] = value
		}
		got := parseLogsFixture(t, vm, "", []map[string]any{entry})
		if got[0].Kind == "success" || got[0].Label != "网络" || got[0].RoutineNetwork {
			t.Fatal("historical account status upgraded HTTP receipt to business success", got)
		}
		for _, status := range []string{"failed", "unknown", "canceled", "interrupted"} {
			entry["operationStatus"] = status
			got := parseLogsFixture(t, vm, "", []map[string]any{entry})
			if !got[0].Abnormal || got[0].Label == "网络" {
				t.Fatal("transport category swallowed operation failure or uncertainty", got)
			}
		}
	}
}

const logProgressComponentFixture = `
Intl.DateTimeFormat=function(){return {format(date){return date.toISOString()}}};
var copied=[],copyReject=false;navigator.clipboard={async writeText(text){if(copyReject)throw new Error("synthetic clipboard denial");copied.push(text);}};
var at="2026-11-24T01:59:00Z",props={token:"synthetic",jobId:"one",job:{operationStatus:"running",lastOperationAt:at}};
function event(message,more={}){return {at:"2026-11-24T02:00:00Z",operationAt:at,operation:"reset-2fa",stage:"totp_reset_diagnostic",status:"working",operationStatus:"running",level:"info",message,...more};}
var read={op_ref:"O0123456789abcdef",seq:2,attempt_ref:"A1",stage:"initial_state",reason_code:"REQUEST_COMPLETE",evidence_source:"transport",next_action:"classify_receipt",method:"GET",path:"/backend-api/accounts/mfa_info",dispatch:"response_received",http_status:200,receipt:"complete",duration_ms:653,access:"read"};
var page={id:"one",logs:"",hasMore:true,nextBefore:"older",entries:[
event("old generic normal read",{reset_diagnostic:read}),
event("old generic security write",{reset_diagnostic:{...read,seq:3,attempt_ref:"A2",stage:"activation",method:"POST",path:"/backend-api/accounts/mfa/user/activate_enrollment",access:"write"}}),
event("已记录写前意图，可能已发",{reset_diagnostic:{...read,seq:4,attempt_ref:"A3",reason_code:"DISPATCH_INTENT",receipt:"not_received",dispatch:"possibly_sent",access:"write"}}),
event("退出结果未知，禁止重发",{level:"warning",reset_diagnostic:{...read,seq:5,attempt_ref:"A4",stage:"logout",reason_code:"NO_RESPONSE",receipt:"not_received",dispatch:"possibly_sent",access:"write"}}),
event("本轮结果：新密钥已保存，退出待核实",{level:"warning",reset_diagnostic:{op_ref:read.op_ref,seq:6,stage:"local_commit",reason_code:"RESET_COMPLETE_LOGOUT_UNCONFIRMED",terminal:true,activation_receipt:"accepted",local_commit:"committed",next_action:"review_logout"}}),
event("new authorization saved",{stage:"totp_workflow_relogin",status:"succeeded"})]};
var tree=renderComponent("JobLogs",props);
function rerender(){tree=renderComponent("JobLogs",props);return tree;}
function rawCopy(){findMatching(tree,n=>n.props?.["aria-label"]==="复制当前显示的日志").props.onClick();}
function readableCopy(){findMatching(tree,n=>n.props?.["aria-label"]==="复制可读日志").props.onClick();}
`

func TestFrontendJobLogsProgressFiltersAndCopiesWithoutDiscardingEvidence(t *testing.T) {
	vm := frontendInsightsVM(t, "components/job-logs.jsx")
	frontendPollingRun(t, vm, logProgressComponentFixture)
	frontendPollingRun(t, vm, `assert(findMatching(tree,n=>n.props?.["aria-label"]==="复制可读日志").props.disabled,"unloaded log copy enabled");ok(requests[0],page);`)
	frontendPollingRun(t, vm, `
rerender();assert(findButton(tree,"关键进展").props["aria-pressed"]&&content(tree).includes("显示 5 / 6 条")&&content(tree).includes("已收起 1 条正常网络读取"),"default progress scope or omission count incorrect");
assert(!content(tree).includes("old generic normal read")&&content(tree).includes("old generic security write")&&content(tree).includes("可能已发")&&content(tree).includes("退出结果未知")&&content(tree).includes("2FA步骤结果")&&content(tree).includes("步骤完成"),"progress hid mutation or recovery evidence");
readableCopy();
`)
	frontendPollingRun(t, vm, `
assert(copied[0].includes("复制范围：本次")&&copied[0].includes("筛选：关键进展")&&copied[0].includes("尚有更早页")&&copied[0].includes("已收起 1 条正常网络读取")&&copied[0].includes("筛选未显示 1 条")&&copied[0].includes("[2FA步骤结果]")&&!copied[0].includes('"reset_diagnostic"'),"readable export lost scope or exposed nested raw JSON");
rawCopy();
`)
	frontendPollingRun(t, vm, `
assert(copied[1].includes('"activation_receipt":"accepted"')&&copied[1].includes('"next_action":"review_logout"')&&!copied[1].includes("old generic normal read"),"raw current-filter copy lost recovery fields or ignored selection");
findButton(tree,"显示全部").props.onClick();rerender();assert(content(tree).includes("显示 6 / 6 条")&&content(tree).includes("读取 2FA 设置：收到完整响应（HTTP 200 · 653 ms）")&&content(tree).includes("old generic normal read"),"one-click all lost raw or action summary");rawCopy();
`)
	frontendPollingRun(t, vm, `
assert(copied[2].includes("筛选：全部")&&copied[2].split("\n").length===7&&JSON.parse(copied[2].split("\n")[1]).reset_diagnostic.attempt_ref==="A1","all raw copy omitted an original event or changed JSON contract");
findMatching(tree,n=>n.type==="button"&&content(n).startsWith("异常")).props.onClick();rerender();rawCopy();
`)
	frontendPollingRun(t, vm, `
assert(copied[3].includes("筛选：异常")&&copied[3].includes("退出结果未知")&&copied[3].includes("review_logout")&&!copied[3].includes("old generic security write"),"abnormal copy lost unknown recovery or included routine write");
copyReject=true;readableCopy();
`)
	frontendPollingRun(t, vm, `rerender();assert(content(tree).includes("synthetic clipboard denial"),"copy failure was hidden");unmount();assert(timers.size===0,"log view leaked timers");`)
}

func TestFrontendJobLogsHistoryKeepsFilterAndUnloadedPageNotice(t *testing.T) {
	vm := frontendInsightsVM(t, "components/job-logs.jsx")
	frontendPollingRun(t, vm, logProgressComponentFixture)
	frontendPollingRun(t, vm, `ok(requests[0],page);`)
	frontendPollingRun(t, vm, `rerender();findButton(tree,"加载更早日志").props.onClick();`)
	frontendPollingRun(t, vm, `assert(requests.length===2&&requests[1].url.endsWith("before=older"),"history read used the wrong page");ok(requests[1],{id:"one",logs:"",entries:[event("historical failed storage",{operationAt:"2026-11-23T01:00:00Z",level:"error"})],hasMore:false,warning:"合成截断提示"});`)
	frontendPollingRun(t, vm, `
rerender();assert(findButton(tree,"关键进展").props["aria-pressed"]&&content(tree).includes("historical failed storage")&&content(tree).includes("显示 6 / 7 条")&&content(tree).includes("合成截断提示")&&content(tree).includes("自动更新已暂停"),"earlier history lost filter, warning or pause");readableCopy();
`)
	frontendPollingRun(t, vm, `
assert(copied[0].includes("复制范围：已加载历史")&&copied[0].includes("分页完整性未知")&&!copied[0].includes("已读所有保留页")&&copied[0].includes("读取或归档有警告"),"history export overstated completeness");
findButton(tree,"本次").props.onClick();rerender();assert(!content(tree).includes("historical failed storage")&&content(tree).includes("显示 5 / 6 条"),"current scope retained previous-operation history");
props={...props,archiveId:"recycled",jobId:"two"};rerender();rerender();assert(!content(tree).includes("退出结果未知"),"new account/archive rendered old log evidence");
`)
	frontendPollingRun(t, vm, `ok(requests[2],{logs:"legacy fresh",entries:[]});`)
	frontendPollingRun(t, vm, `rerender();rawCopy();`)
	frontendPollingRun(t, vm, `assert(copied.at(-1).includes("分页完整性未知")&&copied.at(-1).includes("legacy fresh"),"legacy page pretended complete or copied old context");unmount();assert(timers.size===0,"history view leaked timers");`)
}

func TestJobLogsLegacyWrapperKeepsMultiplicityAndExactRawText(t *testing.T) {
	vm := jobLogsVM(t)
	line := "2026-11-24T02:00:00Z [info] legacy progress"
	entries := []map[string]any{{"at": "2026-11-24T02:00:00Z", "level": "info", "message": line, "legacyText": true}, {"at": "2026-11-24T02:00:00Z", "level": "info", "message": line, "legacyText": true}}
	got := parseLogsFixture(t, vm, strings.Join([]string{line, line}, "\n"), entries)
	if len(got) != 2 || got[0].Raw != line || got[1].Raw != line || got[0].Message != "legacy progress" || got[0].Key == got[1].Key {
		t.Fatal("legacy wrappers duplicated or consumed identical real occurrences", got)
	}
	structured := map[string]any{"at": "2026-11-24T03:00:00Z", "level": "info", "message": line}
	got = parseLogsFixture(t, vm, line+"\n"+fmt.Sprintf("%s [info] %s", structured["at"], line), []map[string]any{structured})
	if len(got) != 2 || got[0].Raw != line || !strings.Contains(got[1].Raw, `"message"`) {
		t.Fatal("unmarked structured message swallowed unrelated legacy text", got)
	}
}
