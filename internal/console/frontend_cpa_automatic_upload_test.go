package console

import "testing"

func TestFrontendCPAAutomaticUploadReportsSeparateAuthorizationAndSyncOutcomes(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const base={status:"completed",lastOperationType:"automatic_relogin",operationStatus:"succeeded",uploadTarget:"cpa",uploadAfterRelogin:true,reloginAuthorizationSaved:true,canDownload:true,resultSaved:true};
for(const upload of ["succeeded","failed","unknown","skipped"]) {
 const result=Browser.describeJob({...base,reloginUploadStatus:upload,autoRepairSyncPaused:upload!=="succeeded"});
 assert(result.status.code==="relogin_upload_"+upload,"CPA automatic upload outcome collapsed into login success: "+upload);
 assert(result.summary.includes("CPA")&&(result.summary.includes("新授权")||result.summary.includes("重登授权")),"CPA outcome lost saved authorization or target");
 assert(result.status.tone===(upload==="succeeded"?"success":"warning"),"CPA failure shown as success");
 if(upload!=="succeeded") {
  assert(result.notices.some(note=>note.key==="cpa-auto-upload-paused"&&note.text.includes("手动上传")),"paused repair lost manual recovery instructions");
  assert(!JSON.stringify(result).includes("取消排除后等待下次巡检"),"CPA upload pause promised automatic retry");
 }
 assert(!Browser.uploadBlockedJob({...base,reloginUploadStatus:upload,autoRepairSyncPaused:true}),"saved CPA authorization blocked manual upload");
}
const active=Browser.describeJob({...base,status:"running",operationStatus:"running",reloginUploadStatus:"running",autoRepairSyncPaused:true});
assert(active.status.code==="relogin_upload_running"&&active.status.spinning&&!active.notices.some(note=>note.key==="cpa-auto-upload-paused"),"active CPA upload was not visible or mislabeled paused");
const unavailable=Browser.describeJob({...base,reloginUploadStatus:"pending",autoRepairSyncPaused:true});
assert(unavailable.status.code==="automatic_upload_paused"&&unavailable.status.tone==="warning","unfinished saved upload was advertised as complete");
const staleSub2=Browser.describeJob({...base,reloginUploadStatus:"unknown",autoRepairSyncPaused:true,autoRepairPendingAccountIds:[7],autoRepairSkippedReason:"CPA 自动巡检已关闭"});
assert(staleSub2.status.code==="relogin_upload_unknown"&&!JSON.stringify(staleSub2).includes("取消排除后等待下次巡检"),"stale Sub2 pending IDs changed CPA unknown recovery semantics");
`)
}

func TestFrontendCPAAutomaticStoppedWorkflowKeepsNewAuthorizationVisible(t *testing.T) {
	vm := frontendBrowserVM(t, "jobPresentation")
	frontendPollingRun(t, vm, `
const base={status:"completed",lastOperationType:"automatic_relogin",uploadTarget:"cpa",uploadAfterRelogin:true,reloginAuthorizationSaved:true,canDownload:true,resultSaved:true,autoRepairSyncPaused:true};
for(const operationStatus of ["canceled","interrupted","failed"]) for(const upload of ["unknown","skipped","failed","succeeded"]) {
 const result=Browser.describeJob({...base,operationStatus,reloginUploadStatus:upload});
 assert(result.status.code==="operation_"+operationStatus,"saved authorization hid stopped workflow outcome");
 assert(result.summary.includes("新授权已保存")&&result.summary.includes("CPA")&&result.summary.includes("可下载"),"stopped CPA workflow hid newly saved authorization");
 if(upload==="unknown") assert(result.summary.includes("待核实"),"uncertain upload was flattened into stopped flow");
 if(upload==="skipped") assert(result.summary.includes("未执行"),"unexecuted upload was presented as failure");
}
const skipped=Browser.describeJob({...base,reloginAuthorizationSaved:false,operationStatus:"skipped",reloginUploadStatus:"skipped",autoRepairSkippedReason:"CPA 自动巡检已关闭"});
assert(skipped.status.code==="automatic_relogin_skipped"&&!skipped.summary.includes("新授权")&&!skipped.notices.some(note=>note.key==="relogin-upload"||note.key==="cpa-auto-upload-paused"),"unexecuted relogin invented a new authorization");
const sub2=Browser.describeJob({...base,uploadTarget:"sub2api",operationStatus:"succeeded",reloginUploadStatus:"unknown",autoRepairPendingAccountIds:[7],autoRepairSkippedReason:"套餐排除"});
assert(sub2.status.code==="automatic_upload_paused"&&sub2.summary.includes("取消排除后等待下次巡检"),"CPA-specific repair changed Sub2 recovery semantics");
const manual=Browser.describeJob({...base,lastOperationType:"relogin",operationStatus:"succeeded",reloginUploadStatus:"unknown"});
assert(manual.status.code==="relogin_upload_unknown"&&!manual.notices.some(note=>note.key==="cpa-auto-upload-paused"),"automatic repair pause leaked into manual relogin");
`)
}
