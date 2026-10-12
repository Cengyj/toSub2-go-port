package console

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

type presentationNotice struct {
	Key, Title, Text, Tone, Scope, Details string
}

type jobPresentation struct {
	Status                  struct{ Code, Label, Tone string }
	Summary                 string
	Terminal                bool
	Notices, HistoryNotices []presentationNotice
	Workflow                *struct {
		Status, Label, Error string
		Uncertain            bool
		Steps                []struct{ Key, State, StateLabel string }
	}
	Phone struct{ Visible bool }
	SMS   *struct{ Text, State, Tone string }
}

func presentationVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	entry, err := filepath.Abs("../../web/src/lib/jobPresentation.js")
	if err != nil {
		t.Fatal(err)
	}
	built := api.Build(api.BuildOptions{
		EntryPoints: []string{entry}, Bundle: true, Write: false,
		Format: api.FormatIIFE, GlobalName: "Presentation", Platform: api.PlatformNeutral,
	})
	if len(built.Errors) > 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("presentation bundle failed: %v", built.Errors)
	}
	vm := sobek.New()
	if _, err := vm.RunString(string(built.OutputFiles[0].Contents)); err != nil {
		t.Fatal(err)
	}
	return vm
}

func presentFixture(t *testing.T, vm *sobek.Runtime, fixture map[string]any) jobPresentation {
	t.Helper()
	fn, ok := sobek.AssertFunction(vm.Get("Presentation").ToObject(vm).Get("describeJob"))
	if !ok {
		t.Fatal("missing describeJob export")
	}
	result, err := fn(sobek.Undefined(), vm.ToValue(fixture))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result.Export())
	if err != nil {
		t.Fatal(err)
	}
	var out jobPresentation
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestJobPresentationSeparatesCurrentActionAndSavedAuthorization(t *testing.T) {
	vm := presentationVM(t)
	cases := []struct {
		name                string
		fixture             map[string]any
		code, tone, summary string
	}{
		{"failed password retains grant", map[string]any{"status": "completed", "operationStatus": "failed", "lastOperationType": "add_password", "operationError": "synthetic password failure", "canDownload": true}, "operation_failed", "danger", "已有授权文件仍可下载"},
		{"canceled setup retains grant", map[string]any{"status": "completed", "operationStatus": "canceled", "lastOperationType": "setup_2fa", "operationError": "用户取消了本次操作", "canDownload": true}, "operation_canceled", "neutral", "已取消"},
		{"interrupted password retains checkpoint", map[string]any{"status": "resume_available", "operationStatus": "interrupted", "lastOperationType": "add_password", "operationError": "服务重启", "canResume": true}, "operation_interrupted", "warning", "原授权流程可继续"},
		{"credential warning preserves success", map[string]any{"status": "completed", "operationStatus": "succeeded", "lastOperationType": "relogin", "operationError": "", "credentialWarning": "synthetic credential write warning", "lastError": "synthetic credential write warning", "canDownload": true}, "operation_succeeded", "success", "授权文件可下载"},
		{"legacy ambiguous reminder", map[string]any{"status": "completed", "lastOperationType": "relogin", "lastError": "synthetic credential reminder", "canDownload": true}, "completed_warning", "warning", "授权文件已保存"},
		{"legacy failed setup retains grant", map[string]any{"status": "completed", "lastOperationType": "setup_2fa", "totpSetupError": "synthetic activation failure", "canDownload": true}, "operation_failed", "danger", "已有授权文件仍可下载"},
		{"no output cannot imply grant saved", map[string]any{"status": "completed", "lastOperationType": "initial_authorization", "canDownload": false}, "completed", "success", "尚无可下载"},
		{"reauth is an action rather than account failure", map[string]any{"status": "reauth_required", "lastOperationType": "relogin"}, "reauth_required", "warning", "需要重新登录"},
		{"generic waiting status", map[string]any{"status": "working", "operationStatus": "waiting", "lastOperationType": "relogin"}, "waiting_input", "warning", "请完成当前验证"},
		{"quarantine overrides all apparent successes", map[string]any{"status": "completed", "operationStatus": "succeeded", "lastOperationType": "relogin", "totpRecoveryPending": true, "canDownload": true}, "recovery_required", "warning", "核实并恢复"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := presentFixture(t, vm, tc.fixture)
			if got.Status.Code != tc.code || got.Status.Tone != tc.tone || !strings.Contains(got.Summary, tc.summary) {
				t.Fatalf("unexpected presentation: %+v", got)
			}
		})
	}
	got := presentFixture(t, vm, cases[3].fixture)
	if len(got.Notices) != 1 || got.Notices[0].Key != "credential" || got.Notices[0].Tone != "warning" {
		t.Fatalf("credential warning was lost or duplicated: %+v", got.Notices)
	}
	for index, tone := range []string{"danger", "info", "warning"} {
		got := presentFixture(t, vm, cases[index].fixture)
		if len(got.Notices) != 1 || got.Notices[0].Key != "operation" || got.Notices[0].Tone != tone {
			t.Fatalf("operation notice conflicts with outcome %s: %+v", got.Status.Code, got.Notices)
		}
	}
}

func TestReloginUploadPresentationSeparatesAuthorizationAndSynchronization(t *testing.T) {
	vm := presentationVM(t)
	for _, tc := range []struct {
		name, operationStatus, uploadStatus, code, summary string
		warning                                            bool
	}{
		{"upload in progress", "running", "running", "relogin_upload_running", "新授权已保存", false},
		{"complete", "succeeded", "succeeded", "relogin_upload_succeeded", "同步至 Sub2API", false},
		{"upload failed", "succeeded", "failed", "relogin_upload_failed", "新授权文件可下载", true},
		{"upload result unknown", "succeeded", "unknown", "relogin_upload_unknown", "同步结果待核实", true},
		{"restart before upload", "succeeded", "skipped", "relogin_upload_skipped", "同步未执行", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := map[string]any{"status": "completed", "operationStatus": tc.operationStatus, "lastOperationType": "relogin", "canDownload": true, "uploadAfterRelogin": true, "reloginAuthorizationSaved": true, "reloginUploadStatus": tc.uploadStatus}
			if tc.warning {
				fixture["reloginUploadError"] = "synthetic synchronization failure"
			}
			got := presentFixture(t, vm, fixture)
			if got.Status.Code != tc.code || !strings.Contains(got.Summary, tc.summary) || (len(got.Notices) > 0) != tc.warning {
				t.Fatalf("authorization/upload outcomes conflict: %+v", got)
			}
			fixture["lastOperationType"] = "add_password"
			got = presentFixture(t, vm, fixture)
			if strings.HasPrefix(got.Status.Code, "relogin_upload") || len(got.Notices) != 0 {
				t.Fatalf("old upload state leaked to another action: %+v", got)
			}
		})
	}
}

func TestManualUploadWaitsForCurrentOperationAndCredentialRecovery(t *testing.T) {
	vm := presentationVM(t)
	fn, ok := sobek.AssertFunction(vm.Get("Presentation").ToObject(vm).Get("uploadBlockedJob"))
	if !ok {
		t.Fatal("missing uploadBlockedJob export")
	}
	for _, tc := range []struct {
		name    string
		fixture map[string]any
		blocked bool
	}{
		{"completed authorization", map[string]any{"status": "completed", "operationStatus": "succeeded", "canDownload": true}, false},
		{"failed operation retains downloadable grant", map[string]any{"status": "completed", "operationStatus": "failed", "canDownload": true}, false},
		{"canceled operation retains downloadable grant", map[string]any{"status": "completed", "operationStatus": "canceled", "canDownload": true}, false},
		{"queued relogin retains old grant", map[string]any{"status": "completed", "operationStatus": "queued", "canDownload": true}, true},
		{"running relogin retains old grant", map[string]any{"status": "completed", "operationStatus": "running", "canDownload": true}, true},
		{"waiting verification retains old grant", map[string]any{"status": "completed", "operationStatus": "waiting", "canDownload": true}, true},
		{"queued account without operation status", map[string]any{"status": "queued", "canDownload": true}, true},
		{"running account without operation status", map[string]any{"status": "working", "canDownload": true}, true},
		{"pending 2fa workflow", map[string]any{"status": "completed", "operationStatus": "succeeded", "canDownload": true, "totpWorkflowStatus": "pending"}, true},
		{"running 2fa workflow", map[string]any{"status": "completed", "operationStatus": "succeeded", "canDownload": true, "totpWorkflowStatus": "running"}, true},
		{"2fa recovery pending", map[string]any{"status": "completed", "operationStatus": "succeeded", "canDownload": true, "totpRecoveryPending": true}, true},
		{"2fa credential invalidated", map[string]any{"status": "completed", "operationStatus": "succeeded", "canDownload": true, "totpCredentialInvalidated": true}, true},
		{"finished workflow releases manual upload", map[string]any{"status": "completed", "operationStatus": "succeeded", "canDownload": true, "totpWorkflowStatus": "succeeded"}, false},
		{"failed optional sync can be uploaded manually", map[string]any{"status": "completed", "operationStatus": "succeeded", "lastOperationType": "relogin", "canDownload": true, "uploadAfterRelogin": true, "reloginUploadStatus": "failed"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fn(sobek.Undefined(), vm.ToValue(tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			if got.ToBoolean() != tc.blocked {
				t.Fatalf("manual upload blocked=%v, want %v", got.ToBoolean(), tc.blocked)
			}
		})
	}
}

func TestJobPresentationCurrentLifecycleOverridesRetainedState(t *testing.T) {
	vm := presentationVM(t)
	cases := []struct {
		name                string
		fixture             map[string]any
		code, tone, summary string
		terminal            bool
	}{
		{"running with old completed grant", map[string]any{"status": "completed", "operationStatus": "running", "lastOperationType": "add_password", "prompt": "原授权文件已保存", "canDownload": true}, "running", "active", "正在添加密码", false},
		{"running with imported return state", map[string]any{"status": "imported", "operationStatus": "running", "lastOperationType": "setup_2fa", "prompt": "操作已结束，请选择功能"}, "running", "active", "正在设置 2FA", false},
		{"queued replaces old successful prompt", map[string]any{"status": "completed", "operationStatus": "queued", "lastOperationType": "relogin", "prompt": "授权已完成", "queuePosition": 3}, "queued", "neutral", "前方还有 2 条", false},
		{"waiting replaces old completed status", map[string]any{"status": "completed", "operationStatus": "waiting", "lastOperationType": "relogin", "prompt": "授权已完成"}, "waiting_input", "warning", "请完成当前验证", false},
		{"operation failure terminal despite old working state", map[string]any{"status": "working", "operationStatus": "failed", "lastOperationType": "relogin", "operationError": "synthetic failure"}, "operation_failed", "danger", "未完成", true},
		{"operation success terminal despite old working state", map[string]any{"status": "working", "operationStatus": "succeeded", "lastOperationType": "relogin", "canDownload": true}, "operation_succeeded", "success", "授权文件可下载", true},
		{"new 2fa queue hides finished previous flow", map[string]any{"status": "completed", "operationStatus": "queued", "lastOperationType": "reset_2fa", "prompt": "旧流程已完成", "totpWorkflowStatus": "succeeded", "totpWorkflowStep": "upload"}, "queued", "neutral", "等待重置 2FA", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := presentFixture(t, vm, tc.fixture)
			if got.Status.Code != tc.code || got.Status.Tone != tc.tone || got.Terminal != tc.terminal || !strings.Contains(got.Summary, tc.summary) {
				t.Fatalf("retained account state overrode current operation: %+v", got)
			}
			if !tc.terminal && (strings.Contains(got.Summary, "授权已完成") || strings.Contains(got.Summary, "旧流程已完成")) {
				t.Fatal("stale successful prompt survived")
			}
		})
	}
}

func TestJobPresentationPreservesResponseStatusAndErrorCode(t *testing.T) {
	vm := presentationVM(t)
	got := presentFixture(t, vm, map[string]any{
		"status": "failed", "operationStatus": "failed", "lastOperationType": "relogin",
		"operationError": `HTTP 403: {"error":{"message":"Synthetic verification code is invalid.","code":"incorrect_code"}}`,
	})
	if len(got.Notices) != 1 || !strings.Contains(got.Notices[0].Text, "HTTP 403") || !strings.Contains(got.Notices[0].Text, "incorrect_code") || !strings.Contains(got.Notices[0].Text, "Synthetic verification") {
		t.Fatalf("readable error lost actionable status/code: %+v", got.Notices)
	}
}

func TestJobPresentationScopesOldDiagnosticsAndDeduplicatesCurrentErrors(t *testing.T) {
	vm := presentationVM(t)
	got := presentFixture(t, vm, map[string]any{
		"status": "working", "operationStatus": "running", "lastOperationType": "relogin", "operationError": "",
		"totpWorkflowStatus": "succeeded", "totpWorkflowStep": "upload", "resetTotp": true,
		"totpSetupError": "old synthetic totp error", "totpResetError": "old synthetic reset error",
		"passwordAddError": "old synthetic password error", "phoneError": "old synthetic phone error",
		"currentPhone": "+12025550123", "smsStatus": "waiting_sms", "smsProviderName": "Fixture SMS",
	})
	if got.Workflow != nil || got.Phone.Visible || got.SMS != nil || len(got.Notices) != 0 || len(got.HistoryNotices) != 5 {
		t.Fatalf("old diagnostics contaminated current relogin: %+v", got)
	}
	got = presentFixture(t, vm, map[string]any{
		"status": "phone_otp", "lastOperationType": "relogin", "operationStatus": "waiting", "operationError": "synthetic rejected code",
		"phoneError": "synthetic rejected code", "smsStatus": "error", "smsError": "synthetic rejected code",
		"currentPhone": "+12025550123",
	})
	if len(got.Notices) != 1 || !got.Phone.Visible {
		t.Fatalf("current failure was duplicated or phone was hidden: %+v", got)
	}
	got = presentFixture(t, vm, map[string]any{"status": "working", "stage": "phone_verified", "lastOperationType": "relogin", "smsStatus": "verified"})
	if got.SMS == nil || got.SMS.State != "verified" || got.SMS.Tone != "success" || !strings.Contains(got.SMS.Text, "已通过") {
		t.Fatalf("verified SMS incorrectly looks busy: %+v", got.SMS)
	}
}

func TestJobPresentationTOTPStageOrderAndUncertainUpload(t *testing.T) {
	vm := presentationVM(t)
	fixture := map[string]any{
		"status": "failed", "operationStatus": "failed", "lastOperationType": "reset_2fa", "operationError": "synthetic logout failure",
		"totpWorkflowStatus": "failed", "totpWorkflowStep": "logout", "totpWorkflowError": "synthetic logout failure",
		"totpResetStatus": "activated", "totpResetPhase": "credential_persisted", "resetTotp": true,
		"logoutAllDevicesAfterTotp": true, "logoutAllDevicesStatus": "failed", "logoutAllDevicesError": "synthetic logout failure",
		"reloginAfterTotp": true, "totpReloginStatus": "skipped", "uploadAfterTotp": true, "totpUploadStatus": "skipped",
	}
	got := presentFixture(t, vm, fixture)
	if got.Workflow == nil || len(got.Workflow.Steps) != 4 || got.Workflow.Steps[0].State != "succeeded" || got.Workflow.Steps[1].State != "failed" || got.Workflow.Steps[2].State != "skipped" || got.Workflow.Steps[3].State != "skipped" || len(got.Notices) != 1 {
		t.Fatalf("logout failure lost ordered outcomes or repeated error: %+v", got)
	}
	fixture["totpWorkflowStep"], fixture["totpUploadStatus"], fixture["canDownload"] = "upload", "unknown", true
	fixture["logoutAllDevicesStatus"], fixture["logoutAllDevicesError"], fixture["totpReloginStatus"] = "succeeded", "", "succeeded"
	fixture["operationError"], fixture["totpWorkflowError"] = "synthetic write timeout", "synthetic write timeout"
	got = presentFixture(t, vm, fixture)
	if got.Status.Tone != "warning" || got.Status.Label != "结果待核实" || !strings.Contains(got.Summary, "新授权已保存") || !got.Workflow.Uncertain || got.Workflow.Steps[3].State != "unknown" || len(got.Notices) != 1 || got.Notices[0].Tone != "warning" {
		t.Fatalf("uncertain write incorrectly implies authentication failure: %+v", got)
	}
	fixture["totpWorkflowStep"], fixture["totpWorkflowStatus"], fixture["operationStatus"] = "relogin", "interrupted", "interrupted"
	fixture["totpReloginStatus"], fixture["totpUploadStatus"] = "unknown", "skipped"
	got = presentFixture(t, vm, fixture)
	if got.Status.Tone != "warning" || !strings.Contains(got.Summary, "不会自动执行") || got.Workflow.Steps[3].State != "skipped" {
		t.Fatalf("restart outcome implies automatic replay: %+v", got)
	}
}

func TestJobPresentationTOTPPartialCompletionKeepsSavedCredentialAndOneDiagnostic(t *testing.T) {
	vm := presentationVM(t)
	for _, tc := range []struct {
		name, step, operationStatus, workflowStatus, stageStatus, code, tone, summary string
	}{
		{"logout rejection", "logout", "failed", "failed", "failed", "workflow_partial", "warning", "新 2FA 已保存 · 退出设备失败"},
		{"logout uncertain", "logout", "failed", "failed", "unknown", "workflow_partial", "warning", "新 2FA 已保存 · 退出设备结果待核实"},
		{"relogin failure", "relogin", "failed", "failed", "failed", "workflow_partial", "warning", "新 2FA 已保存 · 重登授权失败"},
		{"upload failure", "upload", "failed", "failed", "failed", "workflow_partial", "warning", "新授权已保存 · Sub2API 同步失败"},
		{"canceled logout", "logout", "canceled", "interrupted", "unknown", "operation_canceled", "neutral", "新 2FA 已保存 · 退出设备已取消"},
		{"interrupted relogin", "relogin", "interrupted", "interrupted", "unknown", "operation_interrupted", "warning", "新 2FA 已保存 · 重登授权已中断"},
		{"saved before followup failed", "totp", "failed", "failed", "failed", "workflow_partial", "warning", "新 2FA 已保存 · 后续操作未完成"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := map[string]any{
				"status": "failed", "operationStatus": tc.operationStatus, "lastOperationType": "reset_2fa",
				"totpWorkflowStatus": tc.workflowStatus, "totpWorkflowStep": tc.step,
				"totpResetStatus": "activated", "totpResetPhase": "credential_persisted", "resetTotp": true,
				"logoutAllDevicesAfterTotp": true, "logoutAllDevicesStatus": "succeeded",
				"reloginAfterTotp": true, "totpReloginStatus": "skipped", "uploadAfterTotp": true, "totpUploadStatus": "skipped",
				"operationError":    "新 2FA 已保存，但后续步骤未完成，后续操作已停止",
				"totpWorkflowError": "新 2FA 已保存，但后续步骤未完成，后续操作已停止",
			}
			switch tc.step {
			case "logout":
				fixture["logoutAllDevicesStatus"] = tc.stageStatus
				fixture["logoutAllDevicesError"] = `HTTP 400: {"error":{"message":"Synthetic logout request rejected.","code":"invalid_request"}}`
			case "relogin":
				fixture["totpReloginStatus"] = tc.stageStatus
			case "upload":
				fixture["totpReloginStatus"], fixture["totpUploadStatus"], fixture["canDownload"] = "succeeded", tc.stageStatus, true
			}
			got := presentFixture(t, vm, fixture)
			if got.Status.Code != tc.code || got.Status.Tone != tc.tone || !strings.Contains(got.Summary, tc.summary) || !got.Terminal {
				t.Fatalf("saved credential and stopped stage outcomes conflict: %+v", got)
			}
			if got.Workflow == nil || got.Workflow.Steps[0].State != "succeeded" || len(got.Notices) != 1 || got.Notices[0].Key != "workflow" || got.Notices[0].Details == "" {
				t.Fatalf("partial completion lost saved credential or duplicates its diagnostic: %+v", got)
			}
			if tc.step == "logout" && (!strings.Contains(got.Notices[0].Text, "HTTP 400") || !strings.Contains(got.Notices[0].Details, "invalid_request") || !strings.Contains(got.Notices[0].Details, "后续操作已停止")) {
				t.Fatalf("compact diagnostic lost response status or detailed cause: %+v", got.Notices)
			}
			if tc.step == "upload" {
				fixture["canDownload"] = false
				got = presentFixture(t, vm, fixture)
				if strings.Contains(got.Summary, "新授权已保存") {
					t.Fatalf("missing authorization output appears saved: %+v", got)
				}
			}
		})
	}

	fixture := map[string]any{
		"status": "failed", "operationStatus": "failed", "lastOperationType": "reset_2fa", "operationError": "Synthetic reset failure",
		"totpWorkflowStatus": "failed", "totpWorkflowStep": "totp", "totpResetStatus": "failed", "resetTotp": true,
	}
	got := presentFixture(t, vm, fixture)
	if got.Status.Tone != "danger" || strings.Contains(got.Summary, "已保存") || got.Workflow.Steps[0].State == "succeeded" {
		t.Fatalf("failed reset without committed credential appears partially saved: %+v", got)
	}
	fixture["totpResetPhase"], fixture["totpRecoveryPending"] = "credential_persisted", true
	got = presentFixture(t, vm, fixture)
	if got.Status.Code != "recovery_required" || strings.Contains(got.Summary, "已保存") || got.Workflow.Steps[0].State != "unknown" {
		t.Fatalf("recovery requirement was hidden by a persisted phase: %+v", got)
	}
}

func TestMonitorAndUploadPresentationDoNotPaintPartialResultsGreen(t *testing.T) {
	vm := presentationVM(t)
	if _, err := vm.RunString(`
		const running = Presentation.describeMonitor({configured:true,enabled:true,running:true,lastError:"synthetic previous failure"});
		if (running.status.code !== "running" || running.errorTitle !== "上次巡检提醒" || running.resultTitle !== "上次巡检结果") throw new Error("previous monitor diagnostics appear current");
		const failed = Presentation.describeMonitor({configured:true,enabled:true,running:false,lastError:"synthetic failed check"});
		if (failed.status.tone !== "warning") throw new Error("failed check appears green");
		const partial = Presentation.describeUpload({result:{created:1,updated:2,failed:1,skipped:1}});
		if (partial.tone !== "warning" || !partial.text.includes("新增 1 条") || !partial.text.includes("更新 2 条") || !partial.text.includes("失败 1 条")) throw new Error("partial upload appears successful");
		const failedUpload = Presentation.describeUpload({}, {error:"synthetic upload timeout"});
		if (failedUpload.tone !== "warning") throw new Error("uncertain upload appears complete");
		const skipped = Presentation.describeUpload({skipped:2,result:{created:1,updated:0,failed:0,skipped:1}});
		if (skipped.tone !== "warning" || !skipped.text.includes("跳过 3 条")) throw new Error("skipped upload appears fully successful or loses skipped count");
		const directSkipped = Presentation.describeUpload({created:1,updated:0,skipped:1});
		if (directSkipped.tone !== "warning" || !directSkipped.text.includes("跳过 1 条")) throw new Error("direct upload counts duplicated");
	`); err != nil {
		t.Fatal(err)
	}
}
