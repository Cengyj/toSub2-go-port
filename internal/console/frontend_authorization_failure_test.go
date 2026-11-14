package console

import (
	"strings"
	"testing"
)

const frontendVerificationFailure = "LEGACY_RUNTIME_CHALLENGE: challenge execution failed [stage=parent_execution frame=false child=false init=false extraParams=false execute=false requests=2 last_http=200 parent_errors=1 child_errors=0]: GO_JS_RUNTIME: SCRIPT_EXCEPTION [phase=timer_callback]"

func TestFrontendInitialAuthorizationShowsOptionalUploadOutcome(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
for (const target of ["sub2api", "cpa"]) {
 const name=target==="cpa"?"CPA":"Sub2API";
 const base={lastOperationType:"initial_authorization",uploadAfterRelogin:true,uploadTarget:target,reloginAuthorizationSaved:true,canDownload:true};
 for (const [upload,operation,code] of [["running","running","relogin_upload_running"],["succeeded","succeeded","relogin_upload_succeeded"],["failed","succeeded","relogin_upload_failed"],["unknown","succeeded","relogin_upload_unknown"],["skipped","succeeded","relogin_upload_skipped"]]) {
  const result=Browser.describeJob({...base,status:operation==="running"?"running":"completed",operationStatus:operation,reloginUploadStatus:upload});
  assert(result.operation.type==="initial_authorization"&&result.operation.label==="首次授权","upload display rewrote the operation identity");
  assert(result.status.code===code&&result.summary.includes(name),"first authorization hid target or upload outcome: "+target+"/"+upload);
  assert(result.terminal===(operation==="succeeded"),"optional upload changed operation lifecycle");
 }
 const failed=Browser.describeJob({...base,status:"failed",operationStatus:"failed",reloginAuthorizationSaved:false,reloginUploadStatus:"skipped",canDownload:true,totpResetPhase:"credential_persisted",totpResetCommitted:true,totpActivationVerified:true});
 const notice=failed.notices.find(item=>item.key==="relogin-upload");
 assert(failed.status.code==="operation_failed"&&notice?.text.includes("本次授权未完成")&&notice.text.includes("未执行上传"),"unexecuted upload lost its authorization prerequisite");
 assert(!notice.text.includes("手动上传新授权")&&!notice.text.includes("新授权已保存"),"old grant or historical 2FA evidence invented a fresh authorization");
 const saved=Browser.describeJob({...base,status:"completed",operationStatus:"succeeded",reloginUploadStatus:"skipped"});
 assert(saved.notices.some(item=>item.key==="relogin-upload"&&item.text.includes("新授权已保存")&&item.text.includes("可手动上传")),"confirmed new grant lost manual sync guidance");
 const plain=Browser.describeJob({...base,status:"completed",operationStatus:"succeeded",uploadAfterRelogin:false,reloginUploadStatus:"failed"});
 assert(plain.status.code==="operation_succeeded"&&!plain.notices.some(item=>item.key==="relogin-upload"),"old upload data leaked into authorization without upload");
}
`)
}

func TestFrontendVerificationScriptFailureExplainsAuthorizationAndRetainsCause(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	if err := vm.Set("verificationFailure", frontendVerificationFailure); err != nil {
		t.Fatal(err)
	}
	frontendPollingRun(t, vm, `
for (const type of ["initial_authorization","relogin","automatic_relogin"]) {
 const job={status:"failed",operationStatus:"failed",lastOperationType:type,operationError:verificationFailure,uploadAfterRelogin:true,uploadTarget:"sub2api",reloginUploadStatus:"skipped",reloginUploadError:verificationFailure,reloginAuthorizationSaved:false,canDownload:false,totpResetPhase:"credential_persisted",totpResetCommitted:true,totpActivationVerified:true,totpRecoveryPending:false};
 const before=JSON.stringify(job),result=Browser.describeJob(job),notice=result.notices.find(item=>item.key==="operation");
 for (const text of ["网站验证脚本执行失败","本次授权未完成","上传未开始","展开查看技术详情"]) assert(notice?.text.includes(text),"missing actionable verification context: "+text);
 assert(notice.details===verificationFailure&&notice.tone==="danger","technical cause or failed outcome lost");
 assert(!result.notices.some(item=>item.key==="relogin-upload"),"same authorization exception duplicated as a remote upload failure");
 assert(!JSON.stringify(result.notices).includes("密码错误")&&!JSON.stringify(result.notices).includes("重新重置")&&!notice.text.includes("重试"),"diagnostic invented password cause, 2FA action, or guaranteed recovery");
 assert(JSON.stringify(job)===before&&result.status.code==="operation_failed"&&!result.status.spinning,"presentation mutated authoritative failure or resumed it");
}
for (const cause of ["HTTP 403: forbidden","GO_JS_RUNTIME: SCRIPT_EXCEPTION [phase=timer_callback]","LEGACY_RUNTIME_CHALLENGE: operation canceled: context canceled","LEGACY_RUNTIME_CHALLENGE: solver time budget exhausted: context deadline exceeded"]) {
 const result=Browser.describeJob({status:"failed",operationStatus:"failed",lastOperationType:"relogin",operationError:cause});
 assert(result.notices.some(item=>item.text===cause&&!item.details),"unrelated error was mislabeled as a script exception");
}
const finished=Browser.describeJob({status:"completed",operationStatus:"succeeded",lastOperationType:"relogin",operationError:verificationFailure});
assert(!finished.notices.some(item=>item.key==="operation"),"stale error repainted successful authorization");
`)
}

func TestJobLogsVerificationScriptFailureKeepsSkippedUploadAndRawCause(t *testing.T) {
	vm := jobLogsVM(t)
	for _, stage := range []string{"relogin_upload", "legacy_challenge"} {
		entry := map[string]any{"operation": "initial_authorization", "operationStatus": "running", "stage": stage, "status": "skipped", "level": "warning", "message": frontendVerificationFailure}
		got := parseLogsFixture(t, vm, "", []map[string]any{entry})
		if len(got) != 1 || got[0].Kind != "warning" || !strings.Contains(got[0].Message, "网站验证脚本执行失败") || !strings.Contains(got[0].Message, "本次授权未完成") || !strings.Contains(got[0].Raw, frontendVerificationFailure) {
			t.Fatalf("verification explanation or raw diagnostic lost: %+v", got)
		}
		if strings.Contains(got[0].Message, "上传未开始") != (stage == "relogin_upload") || strings.Contains(got[0].Message, "重试") {
			t.Fatalf("log inferred upload or retry without evidence: %+v", got)
		}
	}
	got := parseLogsFixture(t, vm, "", []map[string]any{{"stage": "relogin_upload", "status": "running", "operationStatus": "running"}})
	if len(got) != 1 || got[0].Kind != "progress" || got[0].Message != "重新授权后的同步" {
		t.Fatalf("upload stage lost its Chinese progress label: %+v", got)
	}
}

func TestFrontendVerificationFailureNoticeExpandsWithoutRetrying(t *testing.T) {
	vm := frontendAutoRepairComponentVM(t, "accounts")
	if err := vm.Set("verificationFailure", frontendVerificationFailure); err != nil {
		t.Fatal(err)
	}
	frontendPollingRun(t, vm, `
const job={id:"synthetic-verification",email:"synthetic-verification@example.invalid",status:"failed",operationStatus:"failed",lastOperationType:"initial_authorization",operationError:verificationFailure,uploadAfterRelogin:true,uploadTarget:"sub2api",reloginAuthorizationSaved:false,reloginUploadStatus:"skipped",reloginUploadError:verificationFailure,canForceRelogin:true,canRetry:true};
const row=RepairUI.JobRow({job,token:"synthetic",forceReloginAvailable:true});
const node=findMatching(row,item=>typeof item.type==="function"&&item.type.name==="JobNotice"&&item.props.notice.key==="operation");
assert(node,"account row lost the website verification explanation");
const notice=node.type(node.props),summary=find(notice,"summary");
assert(notice.type==="details"&&content(summary).includes("本次授权未完成")&&content(summary).includes("上传未开始")&&!content(summary).includes("GO_JS_RUNTIME"),"technical error displaced the readable disclosure summary");
assert(content(notice).includes(verificationFailure)&&summary.props["aria-label"].includes("展开查看完整内容"),"original diagnostic became inaccessible");
assert(findButton(row,"重登授权")&&!findButton(row,"重登授权").props.disabled,"diagnostic presentation disabled manual authorization");
assert(requests.length===0&&timers.size===0,"rendering authorization failure started login or upload work");
`)
}
